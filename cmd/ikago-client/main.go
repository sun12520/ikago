package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/xtaci/kcp-go"
	"ikago/internal/addr"
	"ikago/internal/config"
	"ikago/internal/crypto"
	"ikago/internal/log"
	"ikago/internal/pcap"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type natIndicator struct {
	srcHardwareAddr net.HardwareAddr
	conn            *pcap.RawConn
}

var (
	argListDevs       = flag.Bool("list-devices", false, "List all valid devices in current computer.")
	argConfig         = flag.String("c", "", "Configuration file.")
	argListenDevs     = flag.String("listen-devices", "", "Devices for listening.")
	argUpDev          = flag.String("upstream-device", "", "Device for routing upstream to.")
	argGateway        = flag.String("gateway", "", "Gateway address.")
	argMethod         = flag.String("method", "plain", "Method of encryption.")
	argPassword       = flag.String("password", "", "Password of encryption.")
	argKCP            = flag.Bool("kcp", false, "Enable KCP.")
	argKCPMTU         = flag.Int("kcp-mtu", kcp.IKCP_MTU_DEF, "KCP tuning option mtu.")
	argKCPSendWindow  = flag.Int("kcp-sndwnd", kcp.IKCP_WND_SND, "KCP tuning option sndwnd.")
	argKCPRecvWindow  = flag.Int("kcp-rcvwnd", kcp.IKCP_WND_RCV, "KCP tuning option rcvwnd.")
	argKCPDataShard   = flag.Int("kcp-datashard", 10, "KCP tuning option datashard.")
	argKCPParityShard = flag.Int("kcp-parityshard", 3, "KCP tuning option parityshard.")
	argKCPACKNoDelay  = flag.Bool("kcp-acknodelay", false, "KCP tuning option acknodelay.")
	argKCPNoDelay     = flag.Bool("kcp-nodelay", false, "KCP tuning option nodelay.")
	argKCPInterval    = flag.Int("kcp-interval", kcp.IKCP_INTERVAL, "KCP tuning option interval.")
	argKCPResend      = flag.Int("kcp-resend", 0, "KCP tuning option resend.")
	argKCPNC          = flag.Int("kcp-nc", 0, "KCP tuning option nc.")
	argVerbose        = flag.Bool("v", false, "Print verbose messages.")
	argPublish        = flag.String("publish", "", "ARP publishing address.")
	argUpPort         = flag.Int("p", 0, "Port for routing upstream.")
	argFilters        = flag.String("f", "", "Filters.")
	argServer         = flag.String("s", "", "Server.")
)

var (
	publishIP  net.IP
	filters    []net.Addr
	upPort     uint16
	serverIP   net.IP
	serverPort uint16
	listenDevs []*pcap.Device
	upDev      *pcap.Device
	gatewayDev *pcap.Device
	crypt      crypto.Crypt
	isKCP      bool
	kcpConfig  *config.KCPConfig
)

var (
	isClosed    bool
	listenConns []*pcap.RawConn
	upConn      net.Conn
	c           chan pcap.ConnPacket
	natLock     sync.RWMutex
	nat         map[string]*natIndicator
)

func init() {
	// Parse arguments
	flag.Parse()

	filters = make([]net.Addr, 0)
	listenDevs = make([]*pcap.Device, 0)

	listenConns = make([]*pcap.RawConn, 0)
	c = make(chan pcap.ConnPacket, 1000)
	nat = make(map[string]*natIndicator)
}

func main() {
	var (
		err     error
		cfg     *config.Config
		gateway net.IP
	)

	// Configuration
	if *argConfig != "" {
		cfg, err = config.ParseFile(*argConfig)
		if err != nil {
			log.Fatalln(fmt.Errorf("parse config file %s: %w", *argConfig, err))
		}
	} else {
		cfg = &config.Config{
			ListenDevs: splitArg(*argListenDevs),
			UpDev:      *argUpDev,
			Gateway:    *argGateway,
			Method:     *argMethod,
			Password:   *argPassword,
			KCP:        *argKCP,
			KCPConfig: config.KCPConfig{
				MTU:         *argKCPMTU,
				SendWindow:  *argKCPSendWindow,
				RecvWindow:  *argKCPRecvWindow,
				DataShard:   *argKCPDataShard,
				ParityShard: *argKCPParityShard,
				ACKNoDelay:  *argKCPACKNoDelay,
				NoDelay:     *argKCPNoDelay,
				Interval:    *argKCPInterval,
				Resend:      *argKCPResend,
				NC:          *argKCPNC,
			},
			Verbose: *argVerbose,
			Publish: *argPublish,
			UpPort:  *argUpPort,
			Filters: splitArg(*argFilters),
			Server:  *argServer,
		}
	}

	// Log
	log.SetVerbose(cfg.Verbose)

	// Exclusive commands
	if *argListDevs {
		log.Infoln("Available devices are listed below, use -listen-devices [devices] or -upstream-device [device] to designate device:")
		devs, err := pcap.FindAllDevs()
		if err != nil {
			log.Fatalln(fmt.Errorf("list devices: %w", err))
		}
		for _, dev := range devs {
			log.Infof("  %s\n", dev)
		}
		os.Exit(0)
	}

	// Verify parameters
	if len(cfg.Filters) <= 0 {
		log.Fatalln("Please provide filters by -f filters.")
	}
	if cfg.Server == "" {
		log.Fatalln("Please provide server by -s ip:port.")
	}
	if cfg.Gateway != "" {
		gateway = net.ParseIP(cfg.Gateway)
		if gateway == nil {
			log.Fatalln(fmt.Errorf("invalid gateway %s", cfg.Gateway))
		}
	}
	if cfg.KCPConfig.MTU > 1500 {
		log.Fatalln(fmt.Errorf("kcp mtu %d out of range", cfg.KCPConfig.MTU))
	}
	if cfg.KCPConfig.SendWindow <= 0 || cfg.KCPConfig.SendWindow > 4294967295 {
		log.Fatalln(fmt.Errorf("kcp send window %d out of range", cfg.KCPConfig.SendWindow))
	}
	if cfg.KCPConfig.RecvWindow <= 0 || cfg.KCPConfig.RecvWindow > 4294967295 {
		log.Fatalln(fmt.Errorf("kcp receive window %d out of range", cfg.KCPConfig.RecvWindow))
	}
	if cfg.KCPConfig.DataShard < 0 {
		log.Fatalln(fmt.Errorf("kcp data shard %d out of range", cfg.KCPConfig.DataShard))
	}
	if cfg.KCPConfig.ParityShard < 0 {
		log.Fatalln(fmt.Errorf("kcp parity shard %d out of range", cfg.KCPConfig.ParityShard))
	}
	if cfg.KCPConfig.Interval < 0 {
		log.Fatalln(fmt.Errorf("kcp interval %d out of range", cfg.KCPConfig.Interval))
	}
	if cfg.KCPConfig.Resend < 0 {
		log.Fatalln(fmt.Errorf("kcp resend %d out of range", cfg.KCPConfig.Resend))
	}
	if cfg.KCPConfig.NC < 0 {
		log.Fatalln(fmt.Errorf("kcp nc %d out of range", cfg.KCPConfig.NC))
	}
	if cfg.UpPort < 0 || cfg.UpPort > 65535 {
		log.Fatalln(fmt.Errorf("upstream port %d out of range", cfg.UpPort))
	}

	// Publish
	if cfg.Publish != "" {
		publishIP = net.ParseIP(cfg.Publish)
		if publishIP == nil {
			log.Fatalln(fmt.Errorf("invalid publish %s", cfg.Publish))
		}
	}

	// Filters
	for _, strFilter := range cfg.Filters {
		f, err := addr.ParseAddr(strFilter)
		if err != nil {
			log.Fatalln(fmt.Errorf("parse filter %s: %w", strFilter, err))
		}
		filters = append(filters, f)
	}

	// Randomize upstream port
	if cfg.UpPort == 0 {
		s := rand.NewSource(time.Now().UnixNano())
		r := rand.New(s)
		// Select an upstream port which is different from any port in filters
		for {
			cfg.UpPort = 49152 + r.Intn(16384)
			var exist bool
			for _, f := range filters {
				switch t := f.(type) {
				case *net.IPAddr:
					break
				case *net.TCPAddr:
					if f.(*net.TCPAddr).Port == cfg.UpPort {
						exist = true
					}
				default:
					panic(fmt.Errorf("type %T not support", t))
				}
				if exist {
					break
				}
			}
			if !exist {
				break
			}
		}
	}
	upPort = uint16(cfg.UpPort)

	serverAddr, err := addr.ParseTCPAddr(cfg.Server)
	if err != nil {
		log.Fatalln(fmt.Errorf("parse server %s: %w", cfg.Server, err))
	}
	serverIP = serverAddr.IP
	serverPort = uint16(serverAddr.Port)

	// Crypt
	crypt, err = crypto.ParseCrypt(cfg.Method, cfg.Password)
	if err != nil {
		log.Fatalln(fmt.Errorf("parse crypt: %w", err))
	}
	method := crypt.Method()
	if method != crypto.MethodPlain {
		log.Infof("Encrypt with %s\n", method)
	}

	// KCP
	isKCP = cfg.KCP
	kcpConfig = &cfg.KCPConfig
	if isKCP {
		log.Infoln("Enable KCP")
	}

	if len(filters) == 1 {
		log.Infof("Proxy from %s through :%d to %s\n", filters[0], cfg.UpPort, serverAddr)
	} else {
		log.Info("Proxy:")
		for _, f := range filters {
			log.Infof("\n  %s", f)
		}
		log.Infof(" through :%d to %s\n", cfg.UpPort, serverAddr)
	}

	// Find devices
	listenDevs, err = pcap.FindListenDevs(cfg.ListenDevs)
	if err != nil {
		log.Fatalln(fmt.Errorf("find listen devices: %w", err))
	}
	if len(cfg.ListenDevs) <= 0 {
		// Remove loopback devices by default
		result := make([]*pcap.Device, 0)

		for _, dev := range listenDevs {
			if dev.IsLoop() {
				continue
			}
			result = append(result, dev)
		}

		listenDevs = result
	}
	if len(listenDevs) <= 0 {
		log.Fatalln(errors.New("cannot determine listen device"))
	}

	upDev, gatewayDev, err = pcap.FindUpstreamDevAndGatewayDev(cfg.UpDev, gateway)
	if err != nil {
		log.Fatalln(fmt.Errorf("find upstream device and gateway device: %w", err))
	}
	if upDev == nil && gatewayDev == nil {
		log.Fatalln(errors.New("cannot determine upstream device and gateway device"))
	}
	if upDev == nil {
		log.Fatalln(errors.New("cannot determine upstream device"))
	}
	if gatewayDev == nil {
		log.Fatalln(errors.New("cannot determine gateway device"))
	}

	// Wait signals
	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		closeAll()
		os.Exit(0)
	}()

	// Open pcap
	err = open()
	if err != nil {
		log.Fatalln(fmt.Errorf("open pcap: %w", err))
	}
}

func open() error {
	var err error

	if len(listenDevs) == 1 {
		log.Infof("Listen on %s\n", listenDevs[0])
	} else {
		log.Infoln("Listen on:")
		for _, dev := range listenDevs {
			log.Infof("  %s\n", dev)
		}
	}
	if !gatewayDev.IsLoop() {
		log.Infof("Route upstream from %s to %s\n", upDev, gatewayDev)
	} else {
		log.Infof("Route upstream in %s\n", upDev)
	}
	if publishIP != nil {
		log.Infof("Publish %s\n", publishIP)
	}

	// Filters for listening
	fs := make([]string, 0)
	for _, f := range filters {
		s, err := addr.SrcBPFFilter(f)
		if err != nil {
			return fmt.Errorf("parse filter %s: %w", f, err)
		}

		fs = append(fs, s)
	}
	f := strings.Join(fs, " || ")
	filter := fmt.Sprintf("((tcp || udp) && (%s) && not (src host %s && src port %d)) || (icmp && (%s) && not src host %s)",
		f, serverIP, serverPort, f, serverIP)
	if publishIP != nil {
		filter = filter + fmt.Sprintf(" || ((arp[6:2] = 1) && dst host %s)", publishIP)
	}

	// Handles for listening
	for _, dev := range listenDevs {
		var (
			err  error
			conn *pcap.RawConn
		)

		if dev.IsLoop() {
			conn, err = pcap.CreateRawConn(dev, dev, filter)
		} else {
			conn, err = pcap.CreateRawConn(dev, gatewayDev, filter)
		}
		if err != nil {
			return fmt.Errorf("open listen device %s: %w", conn.LocalDev().Alias(), err)
		}

		listenConns = append(listenConns, conn)
	}

	// Handle for routing upstream
	if isKCP {
		upConn, err = pcap.DialWithKCP(upDev, gatewayDev, upPort, &net.TCPAddr{IP: serverIP, Port: int(serverPort)}, crypt, kcpConfig)
	} else {
		upConn, err = pcap.Dial(upDev, gatewayDev, upPort, &net.TCPAddr{IP: serverIP, Port: int(serverPort)}, crypt)
	}
	if err != nil {
		return fmt.Errorf("open upstream: %w", err)
	}

	// Start handling
	for i := 0; i < len(listenConns); i++ {
		conn := listenConns[i]

		go func() {
			for {
				packet, err := conn.ReadPacket()
				if err != nil {
					if isClosed {
						return
					}
					log.Errorln(fmt.Errorf("read listen device %s: %w", conn.LocalDev().Alias(), err))
					continue
				}

				c <- pcap.ConnPacket{Packet: packet, Conn: conn}
			}
		}()
	}

	go func() {
		for cp := range c {
			err := handleListen(cp.Packet, cp.Conn)
			if err != nil {
				log.Errorln(fmt.Errorf("handle listen in device %s: %w", cp.Conn.LocalDev().Alias(), err))
				log.Verboseln(cp.Packet)
				continue
			}
		}
	}()

	for {
		b := make([]byte, pcap.MaxMTU)

		n, err := upConn.Read(b)
		if err != nil {
			if isClosed {
				return nil
			}
			log.Errorln(fmt.Errorf("read upstream: %w", err))
			continue
		}

		err = handleUpstream(b[:n])
		if err != nil {
			log.Errorln(fmt.Errorf("handle upstream in address %s: %w", upConn.LocalAddr().String(), err))
			log.Verbosef("Source: %s\nSize: %d Bytes\n\n", upConn.RemoteAddr().String(), n)
			continue
		}
	}
}

func closeAll() {
	isClosed = true
	for _, handle := range listenConns {
		if handle != nil {
			handle.Close()
		}
	}
	if upConn != nil {
		upConn.Close()
	}
}

func publish(packet gopacket.Packet, conn *pcap.RawConn) error {
	var (
		indicator    *pcap.PacketIndicator
		arpLayer     *layers.ARP
		newARPLayer  *layers.ARP
		linkLayer    gopacket.Layer
		newLinkLayer *layers.Ethernet
	)

	// Parse packet
	indicator, err := pcap.ParsePacket(packet)
	if err != nil {
		return fmt.Errorf("parse packet: %w", err)
	}

	if t := indicator.NetworkLayer().LayerType(); t != layers.LayerTypeARP {
		return fmt.Errorf("network layer type %s not support", t)
	}

	// Create new ARP layer
	arpLayer = indicator.ARPLayer()
	newARPLayer = &layers.ARP{
		AddrType:          arpLayer.AddrType,
		Protocol:          arpLayer.Protocol,
		HwAddressSize:     arpLayer.HwAddressSize,
		ProtAddressSize:   arpLayer.ProtAddressSize,
		Operation:         layers.ARPReply,
		SourceHwAddress:   conn.LocalDev().HardwareAddr(),
		SourceProtAddress: arpLayer.DstProtAddress,
		DstHwAddress:      arpLayer.SourceHwAddress,
		DstProtAddress:    arpLayer.SourceProtAddress,
	}

	// Create new link layer
	linkLayer = packet.LinkLayer()

	switch t := linkLayer.LayerType(); t {
	case layers.LayerTypeEthernet:
		newLinkLayer = &layers.Ethernet{
			SrcMAC:       conn.LocalDev().HardwareAddr(),
			DstMAC:       linkLayer.(*layers.Ethernet).SrcMAC,
			EthernetType: linkLayer.(*layers.Ethernet).EthernetType,
		}
	default:
		return fmt.Errorf("link layer type %s not support", t)
	}

	// Serialize layers
	data, err := pcap.Serialize(newLinkLayer, newARPLayer)
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	// Write packet data
	n, err := conn.Write(data)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	log.Verbosef("Reply an %s request: %s -> %s (%d Bytes)\n", indicator.NetworkLayer().LayerType(), indicator.SrcIP(), indicator.DstIP(), n)

	return nil
}

func handleListen(packet gopacket.Packet, conn *pcap.RawConn) error {
	var contents []byte

	// Parse packet
	indicator, err := pcap.ParsePacket(packet)
	if err != nil {
		return fmt.Errorf("parse packet: %w", err)
	}

	// ARP
	if indicator.NetworkLayer().LayerType() == layers.LayerTypeARP {
		err := publish(packet, conn)
		if err != nil {
			return fmt.Errorf("publish: %w", err)
		}

		return nil
	}

	// Serialize layers
	if indicator.TransportLayer() == nil {
		contents, err = pcap.SerializeRaw(indicator.NetworkLayer().(gopacket.SerializableLayer),
			gopacket.Payload(indicator.Payload()))
	} else {
		contents, err = pcap.SerializeRaw(indicator.NetworkLayer().(gopacket.SerializableLayer),
			indicator.TransportLayer().(gopacket.SerializableLayer),
			gopacket.Payload(indicator.Payload()))
	}
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	n, err := upConn.Write(contents)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// Record the connection of the packet
	ni, ok := nat[indicator.SrcIP().String()]
	if !ok || ni.srcHardwareAddr.String() != indicator.SrcHardwareAddr().String() {
		natLock.Lock()
		nat[indicator.SrcIP().String()] = &natIndicator{srcHardwareAddr: indicator.SrcHardwareAddr(), conn: conn}
		natLock.Unlock()
	}

	log.Verbosef("Redirect an outbound %s packet: %s -> %s (%d Bytes)\n", indicator.TransportProtocol(), indicator.Src().String(), indicator.Dst().String(), n)

	return nil
}

func handleUpstream(contents []byte) error {
	var (
		embIndicator     *pcap.PacketIndicator
		newLinkLayer     gopacket.Layer
		newLinkLayerType gopacket.LayerType
		data             []byte
	)

	// Empty payload
	if len(contents) <= 0 {
		return errors.New("empty payload")
	}

	// Parse embedded packet
	embIndicator, err := pcap.ParseEmbPacket(contents)
	if err != nil {
		return fmt.Errorf("parse embedded packet: %w", err)
	}

	// Check map
	natLock.RLock()
	ni, ok := nat[embIndicator.DstIP().String()]
	natLock.RUnlock()
	if !ok {
		return fmt.Errorf("missing nat to %s", embIndicator.DstIP())
	}

	// Decide Loopback or Ethernet
	if ni.conn.IsLoop() {
		newLinkLayerType = layers.LayerTypeLoopback
	} else {
		newLinkLayerType = layers.LayerTypeEthernet
	}

	// Create new link layer
	switch newLinkLayerType {
	case layers.LayerTypeLoopback:
		newLinkLayer = pcap.CreateLoopbackLayer()
	case layers.LayerTypeEthernet:
		newLinkLayer, err = pcap.CreateEthernetLayer(ni.conn.LocalDev().HardwareAddr(), ni.srcHardwareAddr, embIndicator.NetworkLayer().(gopacket.NetworkLayer))
	default:
		return fmt.Errorf("link layer type %s not support", newLinkLayerType)
	}
	if err != nil {
		return fmt.Errorf("create link layer: %w", err)
	}

	// Serialize layers
	if embIndicator.TransportLayer() == nil {
		data, err = pcap.SerializeRaw(newLinkLayer.(gopacket.SerializableLayer),
			embIndicator.NetworkLayer().(gopacket.SerializableLayer),
			gopacket.Payload(embIndicator.Payload()))
	} else {
		data, err = pcap.SerializeRaw(newLinkLayer.(gopacket.SerializableLayer),
			embIndicator.NetworkLayer().(gopacket.SerializableLayer),
			embIndicator.TransportLayer().(gopacket.SerializableLayer),
			gopacket.Payload(embIndicator.Payload()))
	}
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	// Write packet data
	n, err := ni.conn.Write(data)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	log.Verbosef("Redirect an inbound %s packet: %s <- %s (%d Bytes)\n", embIndicator.TransportProtocol(), embIndicator.Dst().String(), embIndicator.Src().String(), n)

	return nil
}

func splitArg(s string) []string {
	if s == "" {
		return nil
	}

	result := make([]string, 0)

	strs := strings.Split(s, ",")

	for _, str := range strs {
		result = append(result, strings.Trim(str, " "))
	}

	return result
}
