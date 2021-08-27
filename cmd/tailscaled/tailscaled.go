// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The tailscaled program is the Tailscale client daemon. It's configured
// and controlled via the tailscale CLI program.
//
// It primarily supports Linux, though other systems will likely be
// supported in the future.
package main // import "tailscale.com/cmd/tailscaled"

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-multierror/multierror"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnserver"
	"tailscale.com/logpolicy"
	"tailscale.com/net/dns"
	"tailscale.com/net/socks5/tssocks"
	"tailscale.com/net/tstun"
	"tailscale.com/paths"
	"tailscale.com/types/flagtype"
	"tailscale.com/types/logger"
	"tailscale.com/util/osshare"
	"tailscale.com/version"
	"tailscale.com/version/distro"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/monitor"
	"tailscale.com/wgengine/netstack"
	"tailscale.com/wgengine/router"
)

// defaultTunName returns the default tun device name for the platform.
func defaultTunName() string {
	switch runtime.GOOS {
	case "openbsd":
		return "tun"
	case "windows":
		return "Tailscale"
	case "darwin":
		// "utun" is recognized by wireguard-go/tun/tun_darwin.go
		// as a magic value that uses/creates any free number.
		return "utun"
	case "linux":
		if distro.Get() == distro.Synology {
			// Try TUN, but fall back to userspace networking if needed.
			// See https://github.com/tailscale/tailscale-synology/issues/35
			return "tailscale0,userspace-networking"
		}
	}
	return "tailscale0"
}

var args struct {
	// tunname is a /dev/net/tun tunnel name ("tailscale0"), the
	// string "userspace-networking", "tap:TAPNAME[:BRIDGENAME]"
	// or comma-separated list thereof.
	tunname string

	cleanup    bool
	debug      string
	port       uint16
	statepath  string
	kubesecret string
	socketpath string
	verbose    int
	socksAddr  string // listen address for SOCKS5 server
}

var (
	installSystemDaemon   func([]string) error // non-nil on some platforms
	uninstallSystemDaemon func([]string) error // non-nil on some platforms
)

var subCommands = map[string]*func([]string) error{
	"install-system-daemon":   &installSystemDaemon,
	"uninstall-system-daemon": &uninstallSystemDaemon,
	"debug":                   &debugModeFunc,
}

func main() {
	// We aren't very performance sensitive, and the parts that are
	// performance sensitive (wireguard) try hard not to do any memory
	// allocations. So let's be aggressive about garbage collection,
	// unless the user specifically overrides it in the usual way.
	if _, ok := os.LookupEnv("GOGC"); !ok {
		debug.SetGCPercent(10)
	}

	printVersion := false
	flag.IntVar(&args.verbose, "verbose", 0, "log verbosity level; 0 is default, 1 or higher are increasingly verbose")
	flag.BoolVar(&args.cleanup, "cleanup", false, "clean up system state and exit")
	flag.StringVar(&args.debug, "debug", "", "listen address ([ip]:port) of optional debug server")
	flag.StringVar(&args.socksAddr, "socks5-server", "", `optional [ip]:port to run a SOCK5 server (e.g. "localhost:1080")`)
	flag.StringVar(&args.tunname, "tun", defaultTunName(), `tunnel interface name; use "userspace-networking" (beta) to not use TUN`)
	flag.Var(flagtype.PortValue(&args.port, 0), "port", "UDP port to listen on for WireGuard and peer-to-peer traffic; 0 means automatically select")
	flag.StringVar(&args.statepath, "state", paths.DefaultTailscaledStateFile(), "path of state file")
	flag.StringVar(&args.socketpath, "socket", paths.DefaultTailscaledSocket(), "path of the service unix socket")
	flag.BoolVar(&printVersion, "version", false, "print version information and exit")
	flag.StringVar(&args.kubesecret, "kube-secret", "", "use kubernetes secrets as the state store")

	if len(os.Args) > 1 {
		sub := os.Args[1]
		if fp, ok := subCommands[sub]; ok {
			if *fp == nil {
				log.SetFlags(0)
				log.Fatalf("%s not available on %v", sub, runtime.GOOS)
			}
			if err := (*fp)(os.Args[2:]); err != nil {
				log.SetFlags(0)
				log.Fatal(err)
			}
			return
		}
	}

	if beWindowsSubprocess() {
		return
	}

	flag.Parse()
	if flag.NArg() > 0 {
		log.Fatalf("tailscaled does not take non-flag arguments: %q", flag.Args())
	}

	if printVersion {
		fmt.Println(version.String())
		os.Exit(0)
	}

	if runtime.GOOS == "darwin" && os.Getuid() != 0 && !strings.Contains(args.tunname, "userspace-networking") && !args.cleanup {
		log.SetFlags(0)
		log.Fatalf("tailscaled requires root; use sudo tailscaled (or use --tun=userspace-networking)")
	}

	if args.socketpath == "" && runtime.GOOS != "windows" {
		log.SetFlags(0)
		log.Fatalf("--socket is required")
	}

	err := run()

	// Remove file sharing from Windows shell (noop in non-windows)
	osshare.SetFileSharingEnabled(false, logger.Discard)

	if err != nil {
		// No need to log; the func already did
		os.Exit(1)
	}
}

func trySynologyMigration(p string) error {
	if runtime.GOOS != "linux" || distro.Get() != distro.Synology {
		return nil
	}

	fi, err := os.Stat(p)
	if err == nil && fi.Size() > 0 || !os.IsNotExist(err) {
		return err
	}
	// File is empty or doesn't exist, try reading from the old path.

	const oldPath = "/var/packages/Tailscale/etc/tailscaled.state"
	if _, err := os.Stat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if err := os.Chown(oldPath, os.Getuid(), os.Getgid()); err != nil {
		return err
	}
	if err := os.Rename(oldPath, p); err != nil {
		return err
	}
	return nil
}

func ipnServerOpts() (o ipnserver.Options) {
	// Allow changing the OS-specific IPN behavior for tests
	// so we can e.g. test Windows-specific behaviors on Linux.
	goos := os.Getenv("TS_DEBUG_TAILSCALED_IPN_GOOS")
	if goos == "" {
		goos = runtime.GOOS
	}

	o.Port = 41112
	o.StatePath = args.statepath
	o.SocketPath = args.socketpath // even for goos=="windows", for tests
	o.KubeSecret = args.kubesecret

	switch goos {
	default:
		o.SurviveDisconnects = true
		o.AutostartStateKey = ipn.GlobalDaemonStateKey
	case "windows":
		// Not those.
	}
	return o
}

func run() error {
	var err error

	pol := logpolicy.New("tailnode.log.tailscale.io")
	pol.SetVerbosityLevel(args.verbose)
	defer func() {
		// Finish uploading logs after closing everything else.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		pol.Shutdown(ctx)
	}()

	if isWindowsService() {
		// Run the IPN server from the Windows service manager.
		log.Printf("Running service...")
		if err := runWindowsService(pol); err != nil {
			log.Printf("runservice: %v", err)
		}
		log.Printf("Service ended.")
		return nil
	}

	var logf logger.Logf = log.Printf
	if v, _ := strconv.ParseBool(os.Getenv("TS_DEBUG_MEMORY")); v {
		logf = logger.RusagePrefixLog(logf)
	}
	logf = logger.RateLimitedFn(logf, 5*time.Second, 5, 100)

	if args.cleanup {
		if os.Getenv("TS_PLEASE_PANIC") != "" {
			panic("TS_PLEASE_PANIC asked us to panic")
		}
		dns.Cleanup(logf, args.tunname)
		router.Cleanup(logf, args.tunname)
		return nil
	}

	if args.statepath == "" && args.kubesecret == "" {
		log.Fatalf("--state is required")
	}
	if err := trySynologyMigration(args.statepath); err != nil {
		log.Printf("error in synology migration: %v", err)
	}

	var debugMux *http.ServeMux
	if args.debug != "" {
		debugMux = newDebugMux()
		go runDebugServer(debugMux, args.debug)
	}

	linkMon, err := monitor.New(logf)
	if err != nil {
		log.Fatalf("creating link monitor: %v", err)
	}
	pol.Logtail.SetLinkMonitor(linkMon)

	var socksListener net.Listener
	if args.socksAddr != "" {
		var err error
		socksListener, err = net.Listen("tcp", args.socksAddr)
		if err != nil {
			log.Fatalf("SOCKS5 listener: %v", err)
		}
		if strings.HasSuffix(args.socksAddr, ":0") {
			// Log kernel-selected port number so integration tests
			// can find it portably.
			log.Printf("SOCKS5 listening on %v", socksListener.Addr())
		}
	}

	e, useNetstack, err := createEngine(logf, linkMon)
	if err != nil {
		logf("wgengine.New: %v", err)
		return err
	}

	var ns *netstack.Impl
	if useNetstack || wrapNetstack {
		onlySubnets := wrapNetstack && !useNetstack
		ns = mustStartNetstack(logf, e, onlySubnets)
	}

	if socksListener != nil {
		srv := tssocks.NewServer(logger.WithPrefix(logf, "socks5: "), e, ns)
		go func() {
			log.Fatalf("SOCKS5 server exited: %v", srv.Serve(socksListener))
		}()
	}

	e = wgengine.NewWatchdog(e)

	ctx, cancel := context.WithCancel(context.Background())
	// Exit gracefully by cancelling the ipnserver context in most common cases:
	// interrupted from the TTY or killed by a service manager.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	// SIGPIPE sometimes gets generated when CLIs disconnect from
	// tailscaled. The default action is to terminate the process, we
	// want to keep running.
	signal.Ignore(syscall.SIGPIPE)
	go func() {
		select {
		case s := <-interrupt:
			logf("tailscaled got signal %v; shutting down", s)
			cancel()
		case <-ctx.Done():
			// continue
		}
	}()

	opts := ipnServerOpts()
	opts.DebugMux = debugMux
	err = ipnserver.Run(ctx, logf, pol.PublicID.String(), ipnserver.FixedEngine(e), opts)
	// Cancelation is not an error: it is the only way to stop ipnserver.
	if err != nil && err != context.Canceled {
		logf("ipnserver.Run: %v", err)
		return err
	}

	return nil
}

func createEngine(logf logger.Logf, linkMon *monitor.Mon) (e wgengine.Engine, useNetstack bool, err error) {
	if args.tunname == "" {
		return nil, false, errors.New("no --tun value specified")
	}
	var errs []error
	for _, name := range strings.Split(args.tunname, ",") {
		logf("wgengine.NewUserspaceEngine(tun %q) ...", name)
		e, useNetstack, err = tryEngine(logf, linkMon, name)
		if err == nil {
			return e, useNetstack, nil
		}
		logf("wgengine.NewUserspaceEngine(tun %q) error: %v", name, err)
		errs = append(errs, err)
	}
	return nil, false, multierror.New(errs)
}

var wrapNetstack = shouldWrapNetstack()

func shouldWrapNetstack() bool {
	if e := os.Getenv("TS_DEBUG_WRAP_NETSTACK"); e != "" {
		v, err := strconv.ParseBool(e)
		if err != nil {
			log.Fatalf("invalid TS_DEBUG_WRAP_NETSTACK value: %v", err)
		}
		return v
	}
	if distro.Get() == distro.Synology {
		return true
	}
	switch runtime.GOOS {
	case "windows", "darwin", "freebsd":
		// Enable on Windows and tailscaled-on-macOS (this doesn't
		// affect the GUI clients), and on FreeBSD.
		return true
	}
	return false
}

func tryEngine(logf logger.Logf, linkMon *monitor.Mon, name string) (e wgengine.Engine, useNetstack bool, err error) {
	conf := wgengine.Config{
		ListenPort:  args.port,
		LinkMonitor: linkMon,
	}
	useNetstack = name == "userspace-networking"
	if !useNetstack {
		dev, devName, err := tstun.New(logf, name)
		if err != nil {
			tstun.Diagnose(logf, name)
			return nil, false, err
		}
		conf.Tun = dev
		if strings.HasPrefix(name, "tap:") {
			conf.IsTAP = true
			e, err := wgengine.NewUserspaceEngine(logf, conf)
			return e, false, err
		}

		r, err := router.New(logf, dev, linkMon)
		if err != nil {
			dev.Close()
			return nil, false, err
		}
		d, err := dns.NewOSConfigurator(logf, devName)
		if err != nil {
			return nil, false, err
		}
		conf.DNS = d
		conf.Router = r
		if wrapNetstack {
			conf.Router = netstack.NewSubnetRouterWrapper(conf.Router)
		}
	}
	e, err = wgengine.NewUserspaceEngine(logf, conf)
	if err != nil {
		return nil, useNetstack, err
	}
	return e, useNetstack, nil
}

func newDebugMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

func runDebugServer(mux *http.ServeMux, addr string) {
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func mustStartNetstack(logf logger.Logf, e wgengine.Engine, onlySubnets bool) *netstack.Impl {
	tunDev, magicConn, ok := e.(wgengine.InternalsGetter).GetInternals()
	if !ok {
		log.Fatalf("%T is not a wgengine.InternalsGetter", e)
	}
	ns, err := netstack.Create(logf, tunDev, e, magicConn, onlySubnets)
	if err != nil {
		log.Fatalf("netstack.Create: %v", err)
	}
	if err := ns.Start(); err != nil {
		log.Fatalf("failed to start netstack: %v", err)
	}
	return ns
}
