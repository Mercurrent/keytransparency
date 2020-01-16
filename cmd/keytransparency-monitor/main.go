// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto"
	"crypto/tls"
	"flag"
	"net"
	"time"

	"github.com/golang/glog"
	"github.com/google/trillian/crypto/keys/pem"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"

	"github.com/google/keytransparency/cmd/serverutil"
	"github.com/google/keytransparency/core/fake"
	"github.com/google/keytransparency/core/monitor"
	"github.com/google/keytransparency/core/monitorserver"
	"github.com/google/keytransparency/internal/backoff"

	mopb "github.com/google/keytransparency/core/api/monitor/v1/monitor_go_proto"
	pb "github.com/google/keytransparency/core/api/v1/keytransparency_go_proto"
	tcrypto "github.com/google/trillian/crypto"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
)

var (
	addr        = flag.String("addr", ":8070", "The ip:port combination to listen on")
	metricsAddr = flag.String("metrics-addr", ":8071", "The ip:port to publish metrics on")
	keyFile     = flag.String("tls-key", "genfiles/server.key", "TLS private key file")
	certFile    = flag.String("tls-cert", "genfiles/server.pem", "TLS cert file")

	signingKey         = flag.String("sign-key", "genfiles/monitor_sign-key.pem", "Path to private key PEM for SMH signing")
	signingKeyPassword = flag.String("password", "towel", "Password of the private key PEM file for SMH signing")
	ktURL              = flag.String("kt-url", "localhost:443", "URL of key-server.")
	insecure           = flag.Bool("insecure", false, "Skip TLS checks")
	directoryID        = flag.String("directoryid", "", "KT Directory identifier to monitor")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	// Connect to Key Transparency
	cc, err := dial(*ktURL, *insecure)
	if err != nil {
		glog.Exitf("Error Dialing %v: %v", ktURL, err)
	}
	ktClient := pb.NewKeyTransparencyClient(cc)

	// The first gRPC command might fail while the keyserver is starting up. Retry for up to 1 minute.
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	b := backoff.Backoff{
		Min:    time.Millisecond,
		Max:    time.Second,
		Factor: 1.5,
	}
	var config *pb.Directory
	if err := b.Retry(cctx, func() (err error) {
		config, err = ktClient.GetDirectory(ctx, &pb.GetDirectoryRequest{DirectoryId: *directoryID})
		if err != nil {
			glog.Errorf("GetDirectory(%v/%v): %v", *ktURL, *directoryID, err)
		}
		return
	}, codes.Unavailable); err != nil {
		glog.Exitf("Could not read directory info %v:", err)
	}

	// Read signing key:
	key, err := pem.ReadPrivateKeyFile(*signingKey, *signingKeyPassword)
	if err != nil {
		glog.Exitf("Could not create signer from %v: %v", *signingKey, err)
	}
	signer := tcrypto.NewSigner(0, key, crypto.SHA256)
	store := fake.NewMonitorStorage()

	// Create monitoring background process.
	mon, err := monitor.NewFromDirectory(ktClient, config, signer, store)
	if err != nil {
		glog.Exitf("Failed to initialize monitor: %v", err)
	}

	go func() {
		if err := mon.ProcessLoop(ctx, 0); err != nil {
			glog.Errorf("ProcessLoop: %v", err)
		}
	}()

	// Monitor Server.
	srv := monitorserver.New(store)

	// Create gRPC server.
	creds, err := credentials.NewServerTLSFromFile(*certFile, *keyFile)
	if err != nil {
		glog.Exitf("Failed to load server credentials %v", err)
	}
	grpcServer := grpc.NewServer(
		grpc.Creds(creds),
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
		grpc.UnaryInterceptor(grpc_prometheus.UnaryServerInterceptor),
	)
	mopb.RegisterMonitorServer(grpcServer, srv)
	reflection.Register(grpcServer)
	grpc_prometheus.Register(grpcServer)
	grpc_prometheus.EnableHandlingTimeHistogram()

	lis, conn, done := listen(ctx, *addr, *certFile)
	defer done()

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return serverutil.ServeHTTPMetrics(*metricsAddr, serverutil.Healthz()) })
	g.Go(func() error {
		return serverutil.ServeHTTPAPIAndGRPC(gctx, lis, *keyFile, *certFile,
			grpcServer, conn, mopb.RegisterMonitorHandler)
	})
	glog.Errorf("Monitor exiting: %v", g.Wait())

}

func dial(url string, insecure bool) (*grpc.ClientConn, error) {
	tcreds, err := transportCreds(url, insecure)
	if err != nil {
		return nil, err
	}

	// TODO(ismail): authenticate the monitor to the kt-server:
	return grpc.Dial(url, grpc.WithTransportCredentials(tcreds))
}

func transportCreds(ktURL string, insecure bool) (credentials.TransportCredentials, error) {
	host, _, err := net.SplitHostPort(ktURL)
	if err != nil {
		return nil, err
	}

	if insecure {
		return credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true, // nolint: gosec
		}), nil
	}
	return credentials.NewClientTLSFromCert(nil, host), nil
}

func listen(ctx context.Context, listenAddr, certFile string) (net.Listener, *grpc.ClientConn, func() error) {
	// Listen and create empty grpc client connection.
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		glog.Exitf("error creating TCP listener: %v", err)
	}
	addr := lis.Addr().String()
	glog.Infof("Listening on %v", addr)
	// Non-blocking dial before we start the server.
	tcreds, err := credentials.NewClientTLSFromFile(certFile, "localhost")
	if err != nil {
		glog.Exitf("Failed opening cert file %v: %v", certFile, err)
	}
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(tcreds))
	if err != nil {
		glog.Exitf("error connecting to %v: %v", addr, err)
	}
	return lis, conn, conn.Close
}
