package main

import (
	"context"
	"expvar"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/dgraph-io/badger"
	"github.com/profefe/profefe/pkg/config"
	"github.com/profefe/profefe/pkg/log"
	"github.com/profefe/profefe/pkg/middleware"
	"github.com/profefe/profefe/pkg/profefe"
	"github.com/profefe/profefe/pkg/storage"
	storageBadger "github.com/profefe/profefe/pkg/storage/badger"
	storageS3 "github.com/profefe/profefe/pkg/storage/s3"
	"github.com/profefe/profefe/version"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"golang.org/x/xerrors"
)

func main() {
	printVersion := flag.Bool("version", false, "print version and exit")

	var conf config.Config
	conf.RegisterFlags(flag.CommandLine)

	flag.Parse()

	if *printVersion {
		fmt.Println(version.String())
		os.Exit(1)
	}

	logger, err := conf.Logger.Build()
	if err != nil {
		panic(err)
	}

	if err := run(logger, conf, os.Stdout); err != nil {
		logger.Error(err)
	}
}

func run(logger *log.Logger, conf config.Config, stdout io.Writer) error {
	var (
		sr storage.Reader
		sw storage.Writer
	)
	if conf.Badger.Dir != "" {
		st, closer, err := initBadgerStorage(logger, conf)
		if err != nil {
			return err
		}
		defer closer.Close()
		sr, sw = st, st
	} else if conf.S3.Bucket != "" {
		st, err := initS3Storage(logger, conf)
		if err != nil {
			return err
		}
		sr, sw = st, st
	} else {
		return fmt.Errorf("storage configuration required")
	}

	mux := http.NewServeMux()

	profefe.SetupRoutes(mux, logger, prometheus.DefaultRegisterer, sr, sw)

	setupDebugRoutes(mux)

	// TODO(narqo) hardcoded stdout when setup logging middleware
	h := middleware.LoggingHandler(stdout, mux)
	h = middleware.RecoveryHandler(h)

	server := http.Server{
		Addr:    conf.Addr,
		Handler: h,
	}

	errc := make(chan error, 1)
	go func() {
		logger.Infow("server is running", "addr", server.Addr)
		errc <- server.ListenAndServe()
	}()

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigs:
		logger.Info("exiting")
	case err := <-errc:
		if err != http.ErrServerClosed {
			return xerrors.Errorf("terminated: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), conf.ExitTimeout)
	defer cancel()

	return server.Shutdown(ctx)
}

func initBadgerStorage(logger *log.Logger, conf config.Config) (*storageBadger.Storage, io.Closer, error) {
	opt := badger.DefaultOptions(conf.Badger.Dir)
	db, err := badger.Open(opt)
	if err != nil {
		return nil, nil, xerrors.Errorf("could not open db: %w", err)
	}

	// run values garbage collection, see https://github.com/dgraph-io/badger#garbage-collection
	go func() {
		for {
			err := db.RunValueLogGC(conf.Badger.GCDiscardRatio)
			if err == nil {
				// nil error is not the expected behaviour, because
				// badger returns ErrNoRewrite as an indicator that everything went ok
				continue
			} else if err != badger.ErrNoRewrite {
				logger.Errorw("badger failed to run value log garbage collection", zap.Error(err))
			}
			time.Sleep(conf.Badger.GCInterval)
		}
	}()

	st := storageBadger.New(logger, db, conf.Badger.ProfileTTL)
	return st, db, nil
}

func initS3Storage(logger *log.Logger, conf config.Config) (*storageS3.Storage, error) {
	var forcePathStyle bool
	if conf.S3.EndpointURL != "" {
		// should one use custom object storage service (e.g. Minio), path-style addressing needs to be set
		forcePathStyle = true
	}
	sess, err := session.NewSession(&aws.Config{
		Endpoint:         aws.String(conf.S3.EndpointURL),
		DisableSSL:       aws.Bool(conf.S3.DisableSSL),
		Region:           aws.String(conf.S3.Region),
		MaxRetries:       aws.Int(conf.S3.MaxRetries),
		S3ForcePathStyle: aws.Bool(forcePathStyle),
	})
	if err != nil {
		return nil, xerrors.Errorf("could not create s3 session: %w", err)
	}
	return storageS3.New(logger, s3.New(sess), conf.S3.Bucket), nil
}

func setupDebugRoutes(mux *http.ServeMux) {
	// pprof handlers, see https://github.com/golang/go/blob/release-branch.go1.13/src/net/http/pprof/pprof.go
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))

	// expvar handlers, see https://github.com/golang/go/blob/release-branch.go1.13/src/expvar/expvar.go
	mux.Handle("/debug/vars", expvar.Handler())

	// prometheus handlers
	mux.Handle("/debug/metrics", promhttp.Handler())
}
