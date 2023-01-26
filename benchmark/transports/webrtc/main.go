// forked from
// https://github.com/libp2p/go-libp2p/blob/d5a280e6f6ca48eb99ba9140707c4fad54207c76/examples/echo/main.go

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net/http"
	"net/http/pprof"
	"strings"

	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"

	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	quic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	tcp "github.com/libp2p/go-libp2p/p2p/transport/tcp"
	wrtc "github.com/libp2p/go-libp2p/p2p/transport/webrtc"
	ws "github.com/libp2p/go-libp2p/p2p/transport/websocket"
	wtransport "github.com/libp2p/go-libp2p/p2p/transport/webtransport"

	golog "github.com/ipfs/go-log/v2"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	connectionOpenInterval = 1 * time.Second
	streamOpenInterval     = 100 * time.Millisecond
	writeInterval          = 500 * time.Millisecond
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// LibP2P code uses golog to log messages. They log with different
	// string IDs (i.e. "swarm"). We can control the verbosity level for
	// all loggers with:
	golog.SetAllLoggers(golog.LevelInfo) // Change to INFO for extra info

	// flags used only for listen cmd
	listenPort := flag.Int("l", 9999, "port to listen to, used for listen cmd")
	insecureF := flag.Bool("insecure", false, "use an unencrypted connection, used for listen cmd")
	seedF := flag.Int64("seed", 0, "set random seed for id generation, used for listen cmd")

	// flags used for both cmds
	tcpF := flag.String("t", "webrtc", "use quic instead of webrtc")
	profPortF := flag.Int("profile", 0, "enable Golang pprof over http on the given port (disabled by default)")
	metricIntervalF := flag.Duration("interval", time.Second, "interval at which to track/trace a metric point")
	metricOutputF := flag.String("metrics", "", "wrote metrics to CSV or use 'stdout' for stdout")

	// used for dial cmd only
	connF := flag.Int("c", 1, "total connections to open")

	// used for dial and report cmd only
	streamF := flag.Int("s", 1, "set number of streams")

	// parse all flags
	flag.Parse()

	cmd := strings.ToLower(strings.TrimSpace(flag.Arg(0)))

	var metrics MetricTracker
	if metricsOutput := *metricOutputF; (cmd == "listen" || cmd == "dial") && metricsOutput != "" {
		log.Printf("log metrics to: %s\n", *metricOutputF)
		metricsOutput = strings.TrimSpace(metricsOutput)
		if strings.ToLower(metricsOutput) == "stdout" {
			metrics = NewStdoutMetricTracker(ctx, *metricIntervalF)
		} else {
			if strings.ToLower(metricsOutput) == "csv" {
				if cmd == "dial" {
					metricsOutput = fmt.Sprintf(
						"metrics_%s_%s_c%d_s%d_p%d.csv",
						cmd,
						*tcpF,
						*connF,
						*streamF,
						bti(*profPortF > 0),
					)
				} else {
					metricsOutput = fmt.Sprintf(
						"metrics_%s_%s_e%d_p%d.csv",
						cmd,
						*tcpF,
						bti(!*insecureF),
						bti(*profPortF > 0),
					)
				}
			}
			metrics = NewCSVMetricTracker(ctx, *metricIntervalF, metricsOutput)
		}
	} else {
		metrics = NewNoopMetricTracker(ctx, *metricIntervalF)
	}

	if profilePort := *profPortF; profilePort > 0 {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
			mux.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
			mux.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
			mux.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
			mux.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
			mux.Handle("/debug/pprof/{cmd}", http.HandlerFunc(pprof.Index))
			if err := http.ListenAndServe(fmt.Sprintf(":%d", profilePort), mux); err != nil {
				log.Printf("profile server exited with error: %v", err)
			}
		}()
	}

	switch cmd {
	case "listen":
		// Make a host that listens on the given multiaddress
		ha, err := makeBasicHost(*listenPort, *tcpF, *insecureF, *seedF)
		if err != nil {
			log.Fatal(err)
		}

		startListener(ctx, ha, *listenPort, *insecureF, metrics)
		// Run until canceled.
		<-ctx.Done()

	case "dial":
		targetAddr := flag.Arg(1)
		if targetAddr == "" {
			panic("target address missing")
		}

		var wg sync.WaitGroup
		for i := 0; i < *connF; i++ {
			go runSender(ctx, targetAddr, *tcpF, *streamF, &wg, metrics)
			time.Sleep(connectionOpenInterval)
		}
		wg.Wait()

	case "report":
		csvFilePath := flag.Arg(1)
		if csvFilePath == "" {
			panic("csv file path missing")
		}
		metrics, err := ReadCsvMetrics(csvFilePath)
		if err != nil {
			panic(err)
		}
		PrintMetricStats(metrics, uint32(*streamF))

	default:
		panic(fmt.Sprintf("unexpected command: %s", cmd))
	}
}

func bti(b bool) int {
	if b {
		return 1
	}
	return 0
}

// makeBasicHost creates a LibP2P host with a random peer ID listening on the
// given multiaddress. It won't encrypt the connection if insecure is true.
func makeBasicHost(listenPort int, tpt string, insecure bool, randseed int64, opts ...libp2p.Option) (host.Host, error) {
	var r io.Reader
	if randseed == 0 {
		r = rand.Reader
	} else {
		r = mrand.New(mrand.NewSource(randseed))
	}

	// Generate a key pair for this host. We will use it at least
	// to obtain a valid host ID.
	priv, _, err := crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, r)
	if err != nil {
		return nil, err
	}

	// setup infinite limits
	mgr, err := rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(rcmgr.InfiniteLimits))
	if err != nil {
		panic(err)
	}

	options := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.DisableRelay(),
		libp2p.ResourceManager(mgr),
	}

	options = append(options, opts...)

	var (
		fmtStr    string
		transport libp2p.Option
	)

	switch tpt {
	case "webrtc":
		fmtStr = "/ip4/0.0.0.0/udp/%d/webrtc"
		transport = libp2p.Transport(wrtc.New)
	case "quic":
		fmtStr = "/ip4/0.0.0.0/udp/%d/quic"
		transport = libp2p.Transport(quic.NewTransport)
	case "webtransport":
		fmtStr = "/ip4/0.0.0.0/udp/%d/quic-v1/webtransport"
		transport = libp2p.Transport(wtransport.New)
	case "tcp":
		fmtStr = "/ip4/0.0.0.0/tcp/%d"
		transport = libp2p.Transport(tcp.NewTCPTransport)
	case "websocket":
		fmtStr = "/ip4/0.0.0.0/tcp/%d/ws"
		transport = libp2p.Transport(ws.New)
	default:
		panic("bad transport: " + tpt)
	}

	options = append(options,
		transport,
		libp2p.ListenAddrStrings(fmt.Sprintf(fmtStr, listenPort)),
	)

	if insecure {
		options = append(options, libp2p.NoSecurity)
	}

	return libp2p.New(options...)
}

func getHostAddress(ha host.Host) string {
	// Build host multiaddress
	hostAddr, err := ma.NewMultiaddr(fmt.Sprintf("/p2p/%s", ha.ID().Pretty()))
	if err != nil {
		log.Fatalf("invalid host address: %v", err)
	}

	// Now we can build a full multiaddress to reach this host
	// by encapsulating both addresses:
	if len(ha.Addrs()) == 0 {
		return hostAddr.String()
	}
	addr := ha.Addrs()[0]
	return addr.Encapsulate(hostAddr).String()
}

func startListener(ctx context.Context, ha host.Host, listenPort int, insecure bool, metrics MetricTracker) {
	fullAddr := getHostAddress(ha)
	log.Printf("listener: my address: %s\n", fullAddr)

	// Set a stream handler on host A. /echo/1.0.0 is
	// a user-defined protocol name.
	ha.SetStreamHandler("/echo/1.0.0", func(s network.Stream) {
		if err := doEcho(s, metrics); err != nil {
			log.Println("reset stream, echo error: ", err)
			log.Println("calling reset")
			s.Reset()
		} else {
			s.Close()
		}
	})

	log.Println("listening for connections")

}

func runSender(ctx context.Context, targetPeer string, tpt string, streamCount int, wg *sync.WaitGroup, metrics MetricTracker) {
	ha, err := makeBasicHost(0, tpt, false, 1)
	if err != nil {
		panic(err)
	}
	fullAddr := getHostAddress(ha)
	log.Printf("sender: my address: %s\n", fullAddr)

	// Set a stream handler on host A. /echo/1.0.0 is
	// a user-defined protocol name.
	ha.SetStreamHandler("/echo/1.0.0", func(s network.Stream) {
		log.Println("sender received new stream")
		if err := doEcho(s, metrics); err != nil {
			log.Println("error echo: ", err)
			s.Reset()
		} else {
			log.Println("sender closing")
			s.Close()
		}
	})

	// Turn the targetPeer into a multiaddr.
	maddr, err := ma.NewMultiaddr(targetPeer)
	if err != nil {
		log.Println("bad multiaddr: ", err)
		return
	}

	// Extract the peer ID from the multiaddr.
	info, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		log.Printf("sender:  peer.AddrInfoFromP2pAddr: %v\n", err)
		return
	}

	log.Printf("sender: %v\n", info)

	// We have a peer ID and a targetAddr so we add it to the peerstore
	// so LibP2P knows how to contact it
	ha.Peerstore().AddAddrs(info.ID, info.Addrs, peerstore.PermanentAddrTTL)

	log.Println("sender opening connection")

	sendStr := strings.Builder{}
	for i := 0; i < 1023; i++ {
		sendStr.WriteRune('0')
	}
	sendStr.WriteRune('\n')

	for i := 0; i < streamCount; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			defer func() {
				log.Printf("exiting stream number: %d\n", idx)
				metrics.SubIncomingStream()
			}()
			metrics.AddIncomingStream()
			log.Printf("processing stream number: %d\n", idx)

			// make a new stream from host B to host A
			// it should be handled on host A by the handler we set above because
			// we use the same /echo/1.0.0 protocol
			s, err := ha.NewStream(context.Background(), info.ID, "/echo/1.0.0")
			if err != nil {
				log.Printf("error opening stream: %v\n", err)
				return
			}

			reader := bufio.NewReader(s)
			for {
				s.SetDeadline(time.Now().Add(5 * time.Second))
				n, err := s.Write([]byte(sendStr.String()))
				if err != nil {
					log.Printf("[%d] error writing to remote: %v\n", idx, err)
					return
				}
				metrics.AddBytesWritten(uint64(n))

				str, err := reader.ReadString('\n')
				if err != nil {
					log.Printf("[%d] error reading from remote: %v\n", idx, err)
					return
				}
				metrics.AddBytesRead(uint64(len(str)))
				time.Sleep(writeInterval)
			}
		}()
		time.Sleep(streamOpenInterval)
	}
}

// doEcho reads a line of data a stream and writes it back
func doEcho(s network.Stream, metrics MetricTracker) error {
	sn := metrics.AddIncomingStream()
	defer func() {
		log.Printf("exiting stream number: %d\n", sn)
		metrics.SubIncomingStream()
	}()

	log.Printf("processing stream number: %d\n", sn)
	buf := bufio.NewReader(s)
	for {
		s.SetDeadline(time.Now().Add(5 * time.Second))
		str, err := buf.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		metrics.AddBytesRead(uint64(len(str)))

		n, err := s.Write([]byte(str))
		if err != nil {
			log.Printf("error sending: %v", err)
			return err
		}
		metrics.AddBytesWritten(uint64(n))
	}
}