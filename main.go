package main

import (
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Noah-Huppert/golog"
	"github.com/go-ping/ping"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PING_COUNT is the number of ping packets sent to determine the average round trip time.
const PING_COUNT int = 1

// log is the application logger.
var log golog.Logger = golog.NewLogger("net-test")

// die will print an error then exit the process with code 1
func die(err error) {
	log.Fatalf("error: %s", err.Error())
}

// check that error is nil, if not print the msg error and exit.
func check(msg string, err error) {
	if err != nil {
		die(fmt.Errorf("error: %s: %s", msg, err.Error()))
	}
}

type StrArrFlag struct {
	data []string
}

func NewStrArrFlag(data []string) StrArrFlag {
	return StrArrFlag{
		data: data,
	}
}

func (a *StrArrFlag) String() string {
	return strings.Join(a.data, " ")
}

func (a *StrArrFlag) Set(value string) error {
	a.data = append(a.data, value)

	return nil
}

func (a StrArrFlag) Get() []string {
	return a.data
}

// main runs the command line interface.
func main() {
	// Flags
	targetHosts := NewStrArrFlag([]string{})
	flag.Var(&targetHosts,
		"t",
		"Target hosts (DNS or IP4) to measure (can be provided multiple times)")

	var primaryTargetHost string
	flag.StringVar(&primaryTargetHost,
		"T",
		"",
		"Add this target host to the beginning of existing target hosts")

	var metricsHost string
	flag.StringVar(&metricsHost,
		"m",
		":2112",
		"Host on which to serve Prometheus metrics",
	)

	var methodFallover bool
	flag.BoolVar(&methodFallover,
		"f",
		true,
		"Only measure the first target host and fallover to other following target hosts if the measurement fails (incompatible with -a)")

	var methodAll bool
	flag.BoolVar(&methodAll,
		"a",
		false,
		"Measure all target hosts (incompatible with -f)")

	if methodFallover == true && methodAll == true {
		die(fmt.Errorf("options -f (fallover) and -a (all) cannot both be provided"))
	}

	var pingMs int
	flag.IntVar(&pingMs,
		"p",
		10000,
		fmt.Sprintf("Interval in milliseconds at which to perform the ping measurement. Will perform %d ping(s). A value of -1 disables this test. Results recorded to the \"ping_rtt_ms\" and \"ping_failures_total\" metrics with the \"target_host\" label.", PING_COUNT))

	var timeoutMs int
	flag.IntVar(&timeoutMs,
		"o",
		30000,
		"Change timeout of the ping")

	flag.Parse()

	if len(targetHosts.Get()) == 0 {
		targetHosts = NewStrArrFlag([]string{
			"1.1.1.1",
			"8.8.8.8",
			"google.com",
			"wikipedia.org",
		})
	}

	if len(primaryTargetHost) > 0 {
		newHosts := []string{primaryTargetHost}
		for _, host := range targetHosts.Get() {
			newHosts = append(newHosts, host)
		}
		targetHosts = NewStrArrFlag(newHosts)
	}

	// Print some information about what will happen
	log.Infof("starting measurements")
	log.Infof("will measure hosts: %s", targetHosts.String())

	if pingMs > 0 {
		log.Infof("will perform ICMP ping measurement (may require sudo)")
	}

	// Monitor target hosts via prometheus
	if pingMs > 0 {
		// Setup prometheus metric
		pingRtt := prom.NewHistogramVec(
			prom.HistogramOpts{
				Name: "ping_rtt_ms",
				Help: "Round trip time for a target host in milliseconds",
				Buckets: []float64{
					0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100,
					200, 400, 600, 800, 1000,
					5000, 10000,
					20000, 30000,
				},
			},
			[]string{"target_host"},
		)
		pingFailures := prom.NewCounterVec(
			prom.CounterOpts{
				Name: "ping_failures_total",
				Help: "Failures in pings for target hosts",
			},
			[]string{"target_host"},
		)

		prom.MustRegister(pingRtt)
		prom.MustRegister(pingFailures)

		// Perform measurement
		go func() {
			for {
				pingers := []*ping.Pinger{}
				for _, host := range targetHosts.Get() {
					pinger, err := ping.NewPinger(host)
					if err != nil {
						log.Warnf("failed to create pinger for \"%s\": %s", host, err.Error())
						pingFailures.With(prom.Labels{
							"target_host": pinger.Addr(),
						}).Inc()
					}
					pinger.Count = PING_COUNT
					pinger.SetPrivileged(true)
					pinger.Timeout = time.Duration(timeoutMs) * time.Millisecond

					pingers = append(pingers, pinger)
				}

				for _, pinger := range pingers {
					err := pinger.Run()
					if err != nil {
						// Failed to ping, don't record ping statistics, but do record the failure
						log.Warnf("failed to ping host \"%s\": %s", pinger.Addr(), err.Error())
						pingFailures.With(prom.Labels{
							"target_host": pinger.Addr(),
						}).Inc()
						continue
					}

					// Record ping round trip time
					stats := pinger.Statistics()

					rtt := float64(stats.AvgRtt.Milliseconds())

					pingRtt.With(prom.Labels{
						"target_host": pinger.Addr(),
					}).Observe(rtt)
					log.Debugf("ping measured %f for \"%s\"", rtt, pinger.Addr())

					// If in fallover mode
					if methodFallover {
						// We just measured one host successfully so stop measuring
						break
					}
				}

				// Sleep after measurement
				time.Sleep(time.Duration(pingMs) * time.Millisecond)
			}
		}()
	}

	// Ensure at least one metric is being recorded
	if pingMs < 0 {
		die(fmt.Errorf("at least one metric must be selected to record (one of: -p)"))
	}

	http.Handle("/metrics", promhttp.Handler())

	log.Infof("starting http Prometheus metrics server on \"%s\"", metricsHost)
	err := http.ListenAndServe(metricsHost, nil)
	if err != http.ErrServerClosed {
		die(fmt.Errorf("failed to run http Prometheus metrics server on \"%s\"", metricsHost))
	}
}
