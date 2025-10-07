package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"gopkg.in/yaml.v3"
)

// Config defines the configuration file structure
type SubnetConfig struct {
	Network           string            `yaml:"network"`
	Gateway           string            `yaml:"gateway,omitempty"`
	Range             string            `yaml:"range"`
	LeaseDuration     int               `yaml:"lease_duration"`
	DNSServers        []string          `yaml:"dns_servers,omitempty"`
	ReservedAddresses map[string]string `yaml:"reserved_addresses,omitempty"`
}

type Config struct {
	Interface     string        `yaml:"interface,omitempty"`
	Network       string        `yaml:"network"`
	Gateway       string        `yaml:"gateway,omitempty"`
	Range         string        `yaml:"range"`
	LeaseDuration int           `yaml:"lease_duration"`
	DNSServers    []string      `yaml:"dns_servers,omitempty"`
	ReservedAddresses map[string]string `yaml:"reserved_addresses,omitempty"`
}

// Lease represents a DHCP lease
type Lease struct {
	IP        net.IP
	MAC       net.HardwareAddr
	ExpiresAt time.Time
}

// DHCPServer defines the DHCP server
type DHCPServer struct {
	subnetConfig SubnetConfig
	leases       map[string]*Lease // MAC string to Lease
	availableIPs []net.IP
	mutex        sync.Mutex
	subnetMask   net.IPMask
	gateway      net.IP
	dnsServers   []net.IP
}

// NewDHCPServer creates a new DHCP server instance from a subnet configuration
func NewDHCPServer(subnetConfig SubnetConfig) (*DHCPServer, error) {
	_, ipNet, err := net.ParseCIDR(subnetConfig.Network)
	if err != nil {
		return nil, fmt.Errorf("invalid network CIDR: %w", err)
	}

	// Parse the IP range
	rangeParts := strings.Split(subnetConfig.Range, "-")
	if len(rangeParts) != 2 {
		return nil, fmt.Errorf("invalid range format: %s", subnetConfig.Range)
	}
	startIP := net.ParseIP(rangeParts[0])
	endIP := net.ParseIP(rangeParts[1])
	if startIP == nil || endIP == nil {
		return nil, fmt.Errorf("invalid start or end IP in range: %s", subnetConfig.Range)
	}

	// Collect reserved IPs
	reservedIPs := make(map[string]struct{})
	for _, ip := range subnetConfig.ReservedAddresses {
		reservedIPs[ip] = struct{}{}
	}

	// Initialize available IPs from the range
	availableIPs := []net.IP{}
	for ip := startIP; !ip.Equal(endIP); ip = incIP(ip) {
		if _, exists := reservedIPs[ip.String()]; !exists {
			availableIPs = append(availableIPs, ip)
		}
	}
	if _, exists := reservedIPs[endIP.String()]; !exists {
		availableIPs = append(availableIPs, endIP)
	}

	// Parse DNS servers
	dnsServers := []net.IP{}
	for _, dnsStr := range subnetConfig.DNSServers {
		ip := net.ParseIP(dnsStr)
		if ip != nil {
			dnsServers = append(dnsServers, ip)
		}
	}

	return &DHCPServer{
		subnetConfig: subnetConfig,
		leases:       make(map[string]*Lease),
		availableIPs: availableIPs,
		subnetMask:   ipNet.Mask,
		gateway:      net.ParseIP(subnetConfig.Gateway),
		dnsServers:   dnsServers,
	}, nil
}

// getIPForClient gets an IP address for the client
func (s *DHCPServer) getIPForClient(mac net.HardwareAddr) (net.IP, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	macStr := mac.String()
	leaseDuration := time.Duration(s.subnetConfig.LeaseDuration) * time.Second

	// Check for reserved IP
	if reservedIP, exists := s.subnetConfig.ReservedAddresses[macStr]; exists {
		ip := net.ParseIP(reservedIP)
		if ip == nil {
			return nil, fmt.Errorf("invalid reserved IP for %s", macStr)
		}
		if lease, exists := s.leases[macStr]; exists {
			lease.IP = ip
			lease.ExpiresAt = time.Now().Add(leaseDuration)
		} else {
			s.leases[macStr] = &Lease{
				IP:        ip,
				MAC:       mac,
				ExpiresAt: time.Now().Add(leaseDuration),
			}
		}
		return ip, nil
	}

	// Check for existing lease (even if expired)
	if lease, exists := s.leases[macStr]; exists {
		isAvailable := true
		for otherMac, otherLease := range s.leases {
			if otherMac != macStr && otherLease.IP.Equal(lease.IP) && time.Now().Before(otherLease.ExpiresAt) {
				isAvailable = false
				break
			}
		}
		if isAvailable {
			lease.ExpiresAt = time.Now().Add(leaseDuration)
			return lease.IP, nil
		}
		delete(s.leases, macStr)
	}

	// Clean up expired leases to reclaim IPs
	for mac, lease := range s.leases {
		if time.Now().After(lease.ExpiresAt) {
			isReserved := false
			for _, reservedIP := range s.subnetConfig.ReservedAddresses {
				if lease.IP.String() == reservedIP {
					isReserved = true
					break
				}
			}
			if !isReserved {
				s.availableIPs = append(s.availableIPs, lease.IP)
				delete(s.leases, mac) // Remove expired lease
			}
		}
	}

	// Assign new IP if no reusable lease exists
	if len(s.availableIPs) == 0 {
		return nil, fmt.Errorf("no available IPs")
	}
	ip := s.availableIPs[0]
	s.availableIPs = s.availableIPs[1:]
	newLease := &Lease{
		IP:        ip,
		MAC:       mac,
		ExpiresAt: time.Now().Add(leaseDuration),
	}
	s.leases[macStr] = newLease
	return ip, nil
}

// ServeDHCP handles DHCP requests
func (s *DHCPServer) ServeDHCP(conn net.PacketConn, peer net.Addr, p *dhcpv4.DHCPv4) {
	if p.OpCode != dhcpv4.OpcodeBootRequest {
		return
	}

	log.Printf("Received %s from %s", p.MessageType(), p.ClientHWAddr)

	switch p.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		ip, err := s.getIPForClient(p.ClientHWAddr)
		if err != nil {
			log.Printf("Error getting IP for %s: %v", p.ClientHWAddr, err)
			return
		}

		modifiers := []dhcpv4.Modifier{
			dhcpv4.WithReply(p),
			dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
			dhcpv4.WithYourIP(ip),
			dhcpv4.WithServerIP(s.gateway), // This should be the server's own IP, but gateway is a reasonable substitute for now
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(s.subnetMask)),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Duration(s.subnetConfig.LeaseDuration) * time.Second)),
		}
		if s.gateway != nil {
			modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptRouter(s.gateway)))
		}
		if len(s.dnsServers) > 0 {
			modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptDNS(s.dnsServers...)))
		}

		reply, err := dhcpv4.New(modifiers...)
		if err != nil {
			log.Printf("Failed to create OFFER: %v", err)
			return
		}
		log.Printf("Offering IP %s to %s", ip, p.ClientHWAddr)
		if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
			log.Printf("Failed to send OFFER: %v", err)
		}

	case dhcpv4.MessageTypeRequest:
		ip, err := s.getIPForClient(p.ClientHWAddr)
		if err != nil {
			log.Printf("Error getting IP for %s: %v", p.ClientHWAddr, err)
			return
		}

		modifiers := []dhcpv4.Modifier{
			dhcpv4.WithReply(p),
			dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
			dhcpv4.WithYourIP(ip),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(s.subnetMask)),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Duration(s.subnetConfig.LeaseDuration) * time.Second)),
		}
		if s.gateway != nil {
			modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptRouter(s.gateway)))
		}
		if len(s.dnsServers) > 0 {
			modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptDNS(s.dnsServers...)))
		}

		reply, err := dhcpv4.New(modifiers...)
		if err != nil {
			log.Printf("Failed to create ACK: %v", err)
			return
		}
		log.Printf("Assigned IP %s to %s", ip, p.ClientHWAddr)
		if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
			log.Printf("Failed to send ACK: %v", err)
		}
	}
}

// wasFlagPassed checks if a flag was explicitly set on the command line
func wasFlagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func main() {
	// Define command-line flag for network interface
	ifaceFlag := flag.String("iface", "en5", "Network interface to bind the DHCP server to")
	configFile := flag.String("config", "dhcp_config.yaml", "Path to the DHCP configuration file")
	flag.Parse()

	// Read and parse the configuration file
	configData, err := os.ReadFile(*configFile)
	if err != nil {
		log.Fatalf("Failed to read config file: %v", err)
	}

	var config Config
	if err := yaml.Unmarshal(configData, &config); err != nil {
		log.Fatalf("Failed to parse config file: %v", err)
	}

	if config.Network == "" {
		log.Fatal("No network configured in the config file")
	}

	// Determine which interface to use. Precedence: command-line > config file > default
	var ifaceToUse string
	defaultValue := "en5"
	if config.Interface != "" {
		ifaceToUse = config.Interface // Use value from config file
	} else {
		ifaceToUse = defaultValue // Use default
	}
	if wasFlagPassed("iface") {
		ifaceToUse = *ifaceFlag // Flag overrides everything
	}

	// Convert config to SubnetConfig
	subnetConfig := SubnetConfig{
		Network:           config.Network,
		Gateway:           config.Gateway,
		Range:             config.Range,
		LeaseDuration:     config.LeaseDuration,
		DNSServers:        config.DNSServers,
		ReservedAddresses: config.ReservedAddresses,
	}

	// Initialize DHCP server
	server, err := NewDHCPServer(subnetConfig)
	if err != nil {
		log.Fatal(err)
	}

	// Set up UDP address for DHCP server
	addr := &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 67}
	s, err := server4.NewServer(ifaceToUse, addr, server.ServeDHCP)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Starting DHCP server on interface %s, port 67...", ifaceToUse)
	if err := s.Serve(); err != nil {
		log.Fatal(err)
	}
}

func incIP(ip net.IP) net.IP {
	newIP := make(net.IP, len(ip))
	copy(newIP, ip)
	for j := len(newIP) - 1; j >= 0; j-- {
		newIP[j]++
		if newIP[j] > 0 {
			break
		}
	}
	return newIP
}

