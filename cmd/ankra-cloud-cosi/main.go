// Command ankra-cloud-cosi is the COSI driver cosi.ankra.cloud: one replica next to the objectstorage provisioner
// sidecar, which turns BucketClaims and BucketAccesses into the driver's calls over a unix socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
	"github.com/ankraio/ankra-cloud-csi/internal/cosi"
	"github.com/ankraio/ankra-cloud-csi/internal/driver"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if runError := run(); runError != nil {
		fmt.Fprintln(os.Stderr, "ankra-cloud-cosi:", runError)
		os.Exit(1)
	}
}

func run() error {
	defaultEndpoint := os.Getenv("COSI_ENDPOINT")
	if defaultEndpoint == "" {
		defaultEndpoint = cosi.DefaultEndpoint
	}
	endpoint := flag.String("endpoint", defaultEndpoint, "COSI endpoint (unix:// or tcp://); COSI_ENDPOINT when set")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("driver", cosi.Name)
	api, clientError := cloud.NewClient(cloud.Configuration{
		Endpoint:     os.Getenv("ANKRA_CLOUD_API_URL"),
		Token:        os.Getenv("ANKRA_CLOUD_TOKEN"),
		CABundlePath: os.Getenv("ANKRA_CLOUD_CA_BUNDLE"),
		UserAgent:    "ankra-cloud-cosi/" + version,
	})
	if clientError != nil {
		return clientError
	}
	cosiDriver, driverError := cosi.New(cosi.Options{API: api, Logger: logger})
	if driverError != nil {
		return driverError
	}
	listener, listenError := driver.Listen(*endpoint)
	if listenError != nil {
		return listenError
	}
	server := cosiDriver.NewServer()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		logger.Info("stopping")
		server.GracefulStop()
	}()
	logger.Info("serving", "endpoint", *endpoint, "version", version)
	if serveError := server.Serve(listener); serveError != nil && !errors.Is(serveError, context.Canceled) {
		return serveError
	}
	return nil
}
