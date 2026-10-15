// Command previa-egress is the forward proxy that is the only route out of
// Previa's build and preview networks, and the origin of readiness probes into
// previews. It runs in its own container (see internal/platform/egress):
//
//	previa-egress [serve] [-listen :3128] [-state DIR]
//	previa-egress probe -target IP:PORT [-path /health] [-timeout 2s]
//	previa-egress allow -state DIR -address IP [-dest host:port]... [-ports 5432,6379|none]
//	previa-egress sync -state DIR [-network IP=host:port[,host:port]]... [-ports 5432,6379|none]
//	previa-egress stats -state DIR
//	previa-egress meter -network IP -start [-hosts host,.domain]
//	previa-egress meter -network IP -end
//
// serve also forwards raw TCP, on the ports the worker last recorded (-ports),
// to the destinations it approved per preview network (allow, sync). probe
// exits 0 when the target is ready, 1 when it is not (printing why) and 2 for
// invalid arguments. meter asks the running proxy, over loopback, to count a
// build network's traffic (optionally limited to hosts) and, at -end, prints
// the totals as JSON.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/platform/logging"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "probe":
			os.Exit(probe(args[1:]))
		case "allow":
			os.Exit(allow(args[1:]))
		case "sync":
			os.Exit(syncAllow(args[1:]))
		case "stats":
			os.Exit(stats(args[1:]))
		case "meter":
			os.Exit(meter(args[1:]))
		}
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	os.Exit(serve(args))
}

func probe(args []string) int {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	target := flags.String("target", "", "IP address and port of the preview")
	path := flags.String("path", "", "HTTP health path; empty checks that the port accepts connections")
	timeout := flags.Duration("timeout", 2*time.Second, "probe timeout")
	if err := flags.Parse(args); err != nil || *target == "" || *timeout <= 0 {
		return 2
	}
	if err := egress.Probe(context.Background(), *target, *path, *timeout); err != nil {
		fmt.Println(err.Error())
		return 1
	}
	fmt.Println("ready")
	return 0
}

// destinations collects repeated -dest flags.
type destinations []egress.Destination

func (d *destinations) String() string { return fmt.Sprint(*d) }
func (d *destinations) Set(value string) error {
	parsed, err := egress.ParseDestination(value)
	if err != nil {
		return err
	}
	*d = append(*d, parsed)
	return nil
}

// allow records the approved destinations of one preview network, named by
// the egress container's address on it; no -dest withdraws them all.
func allow(args []string) int {
	flags := flag.NewFlagSet("allow", flag.ContinueOnError)
	state := flags.String("state", egress.StateDir, "state directory")
	address := flags.String("address", "", "the egress container's IPv4 address on the preview network")
	var approved destinations
	flags.Var(&approved, "dest", "an approved destination, host:port (repeatable)")
	ports := flags.String("ports", "", "the ports to forward, comma-separated, or none; empty leaves them as they are")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	network, err := netip.ParseAddr(*address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "allow: -address must be an IPv4 address")
		return 2
	}
	if err := writePorts(*state, *ports); err != nil {
		fmt.Fprintln(os.Stderr, "allow:", err)
		return 1
	}
	if err := egress.WriteAllowFile(egress.AllowDir(*state), network, approved); err != nil {
		fmt.Fprintln(os.Stderr, "allow:", err)
		return 1
	}
	return 0
}

// writePorts records the forwarded ports when given: a list, or none.
func writePorts(state, value string) error {
	if value == "" {
		return nil
	}
	var ports []int
	if value != "none" {
		var err error
		if ports, err = parsePorts(value); err != nil {
			return err
		}
	}
	return egress.WritePortsFile(egress.PortsPath(state), ports)
}

// networks collects repeated -network IP=host:port,... flags.
type networks map[netip.Addr][]egress.Destination

func (n networks) String() string { return fmt.Sprint(len(n)) }
func (n networks) Set(value string) error {
	address, list, ok := strings.Cut(value, "=")
	network, err := netip.ParseAddr(address)
	if !ok || err != nil {
		return fmt.Errorf("use IP=host:port[,host:port]")
	}
	for item := range strings.SplitSeq(list, ",") {
		d, err := egress.ParseDestination(item)
		if err != nil {
			return err
		}
		n[network] = append(n[network], d)
	}
	return nil
}

// syncAllow makes the allow files exactly the given networks' approvals.
func syncAllow(args []string) int {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	state := flags.String("state", egress.StateDir, "state directory")
	desired := networks{}
	flags.Var(desired, "network", "a preview network's approvals, IP=host:port[,host:port] (repeatable)")
	ports := flags.String("ports", "", "the ports to forward, comma-separated, or none; empty leaves them as they are")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if err := writePorts(*state, *ports); err != nil {
		fmt.Fprintln(os.Stderr, "sync:", err)
		return 1
	}
	if err := egress.SyncAllowFiles(egress.AllowDir(*state), desired); err != nil {
		fmt.Fprintln(os.Stderr, "sync:", err)
		return 1
	}
	return 0
}

// stats prints the forwarding counters as JSON.
func stats(args []string) int {
	flags := flag.NewFlagSet("stats", flag.ContinueOnError)
	state := flags.String("state", egress.StateDir, "state directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	counters, err := egress.ReadStats(egress.StatsPath(*state))
	if err != nil {
		fmt.Fprintln(os.Stderr, "stats:", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(counters); err != nil {
		return 1
	}
	return 0
}

// meter starts or ends metering of one build network.
func meter(args []string) int {
	flags := flag.NewFlagSet("meter", flag.ContinueOnError)
	address := flags.String("network", "", "the egress container's IPv4 address on the build network")
	start := flags.Bool("start", false, "start metering, replacing an earlier registration")
	end := flags.Bool("end", false, "print the totals as JSON and stop metering")
	hosts := flags.String("hosts", "", "with -start: the only hosts the network may reach, comma-separated (.domain matches subdomains)")
	if err := flags.Parse(args); err != nil || *start == *end {
		return 2
	}
	network, err := netip.ParseAddr(*address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "meter: -network must be an IP address")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if *start {
		var list []string
		if *hosts != "" {
			list = strings.Split(*hosts, ",")
		}
		if err := egress.StartMeter(ctx, network, list); err != nil {
			fmt.Fprintln(os.Stderr, "meter:", err)
			return 1
		}
		return 0
	}
	usage, err := egress.EndMeter(ctx, network)
	if err != nil {
		fmt.Fprintln(os.Stderr, "meter:", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(usage); err != nil {
		return 1
	}
	return 0
}

func parsePorts(value string) ([]int, error) {
	var ports []int
	for part := range strings.SplitSeq(value, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid port %q", part)
		}
		ports = append(ports, port)
	}
	return ports, nil
}

// serve runs the proxy until it is told to stop and returns the exit code;
// its deferred cleanup runs before the process exits.
func serve(args []string) int {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := flags.String("listen", ":"+egress.Port, "address to listen on")
	state := flags.String("state", egress.StateDir, "state directory (forwarded ports, allow files and counters)")
	_ = flags.Parse(args)
	logger, err := logging.New(os.Stdout, os.Getenv("LOG_LEVEL"))
	if err != nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           egress.NewProxy(logger),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := egress.NewForwarder(*state, logger).Serve(ctx); err != nil {
			logger.Error("outbound forwarding stopped", "error", err)
			stop()
		}
	}()
	logger.Info("egress proxy listening", "address", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("egress proxy stopped", "error", err)
		return 1
	}
	return 0
}
