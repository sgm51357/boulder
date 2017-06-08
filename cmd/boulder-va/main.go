package main

import (
	"flag"
	"os"
	"strings"
	"time"

	"github.com/jmhodges/clock"

	"github.com/letsencrypt/boulder/bdns"
	"github.com/letsencrypt/boulder/cdr"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/features"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	"github.com/letsencrypt/boulder/va"
)

const clientName = "VA"

type config struct {
	VA struct {
		cmd.ServiceConfig

		UserAgent string

		IssuerDomain string

		PortConfig cmd.PortConfig

		GoogleSafeBrowsing *cmd.GoogleSafeBrowsingConfig

		CAADistributedResolver *cmd.CAADistributedResolverConfig

		// The number of times to try a DNS query (that has a temporary error)
		// before giving up. May be short-circuited by deadlines. A zero value
		// will be turned into 1.
		DNSTries int

		// Feature flag to enable enforcement of CAA SERVFAILs.
		CAASERVFAILExceptions string

		RemoteVAs                   []cmd.GRPCClientConfig
		MaxRemoteValidationFailures int64

		Features map[string]bool
	}

	Syslog cmd.SyslogConfig

	Common struct {
		DNSResolver               string
		DNSTimeout                string
		DNSAllowLoopbackAddresses bool
	}
}

func main() {
	configFile := flag.String("config", "", "File path to the configuration file for this service")
	flag.Parse()
	if *configFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	var c config
	err := cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")

	err = features.Set(c.VA.Features)
	cmd.FailOnError(err, "Failed to set feature flags")

	scope, logger := cmd.StatsAndLogging(c.Syslog)
	defer logger.AuditPanic()
	logger.Info(cmd.VersionString(clientName))

	pc := &cmd.PortConfig{
		HTTPPort:  80,
		HTTPSPort: 443,
		TLSPort:   443,
	}
	if c.VA.PortConfig.HTTPPort != 0 {
		pc.HTTPPort = c.VA.PortConfig.HTTPPort
	}
	if c.VA.PortConfig.HTTPSPort != 0 {
		pc.HTTPSPort = c.VA.PortConfig.HTTPSPort
	}
	if c.VA.PortConfig.TLSPort != 0 {
		pc.TLSPort = c.VA.PortConfig.TLSPort
	}

	var sbc va.SafeBrowsing
	// If the feature flag is set, use the Google safebrowsing library that
	// implements the v4 api instead of the legacy letsencrypt fork of
	// go-safebrowsing-api
	if features.Enabled(features.GoogleSafeBrowsingV4) {
		sbc, err = newGoogleSafeBrowsingV4(c.VA.GoogleSafeBrowsing, logger)
	} else {
		sbc, err = newGoogleSafeBrowsing(c.VA.GoogleSafeBrowsing)
	}
	cmd.FailOnError(err, "Failed to create Google Safe Browsing client")

	var cdrClient *cdr.CAADistributedResolver
	if c.VA.CAADistributedResolver != nil {
		var err error
		cdrClient, err = cdr.New(
			scope,
			c.VA.CAADistributedResolver.Timeout.Duration,
			c.VA.CAADistributedResolver.MaxFailures,
			c.VA.CAADistributedResolver.Proxies,
			logger)
		cmd.FailOnError(err, "Failed to create CAADistributedResolver")
	}

	dnsTimeout, err := time.ParseDuration(c.Common.DNSTimeout)
	cmd.FailOnError(err, "Couldn't parse DNS timeout")
	dnsTries := c.VA.DNSTries
	if dnsTries < 1 {
		dnsTries = 1
	}
	clk := clock.Default()
	caaSERVFAILExceptions, err := bdns.ReadHostList(c.VA.CAASERVFAILExceptions)
	cmd.FailOnError(err, "Couldn't read CAASERVFAILExceptions file")
	var resolver bdns.DNSResolver
	if !c.Common.DNSAllowLoopbackAddresses {
		r := bdns.NewDNSResolverImpl(
			dnsTimeout,
			[]string{c.Common.DNSResolver},
			caaSERVFAILExceptions,
			scope,
			clk,
			dnsTries)
		resolver = r
	} else {
		r := bdns.NewTestDNSResolverImpl(dnsTimeout, []string{c.Common.DNSResolver}, scope, clk, dnsTries)
		resolver = r
	}

	var remotes []va.RemoteVA
	if len(c.VA.RemoteVAs) > 0 {
		tls, err := c.VA.TLS.Load()
		cmd.FailOnError(err, "TLS config")
		for _, rva := range c.VA.RemoteVAs {
			vaConn, err := bgrpc.ClientSetup(&rva, tls, scope)
			cmd.FailOnError(err, "Unable to create remote VA client")
			remotes = append(remotes, va.RemoteVA{bgrpc.NewValidationAuthorityGRPCClient(vaConn), strings.Join(rva.ServerAddresses, ",")})
		}
	}

	vai := va.NewValidationAuthorityImpl(
		pc,
		sbc,
		cdrClient,
		resolver,
		remotes,
		c.VA.MaxRemoteValidationFailures,
		c.VA.UserAgent,
		c.VA.IssuerDomain,
		scope,
		clk,
		logger)

	tls, err := c.VA.TLS.Load()
	cmd.FailOnError(err, "TLS config")
	grpcSrv, l, err := bgrpc.NewServer(c.VA.GRPC, tls, scope)
	cmd.FailOnError(err, "Unable to setup VA gRPC server")
	err = bgrpc.RegisterValidationAuthorityGRPCServer(grpcSrv, vai)
	cmd.FailOnError(err, "Unable to register VA gRPC server")
	go func() {
		err = grpcSrv.Serve(l)
		cmd.FailOnError(err, "VA gRPC service failed")
	}()

	go cmd.CatchSignals(logger, func() {
		if grpcSrv != nil {
			grpcSrv.GracefulStop()
		}
	})

	go cmd.DebugServer(c.VA.DebugAddr)
	go cmd.ProfileCmd(scope)

	select {}
}
