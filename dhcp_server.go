package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"gopkg.in/yaml.v3"
	"os"
)

// Config defines the configuration file structure
type Config struct {
	ReservedAddresses map[string]string `yaml:"reserved_addresses"` // MAC -> IP
	LeaseDuration     int               `yaml:"lease_duration"`     // Lease duration in seconds
}

// Lease represents a DHCP lease
type Lease struct {
	IP        net.IP
	MAC       net.HardwareAddr
	ExpiresAt time.Time
}

// DHCPServer defines the DHCP server
type DHCPServer struct {
	config        Config
	leases        map[string]*Lease // MAC string to Lease
	availableIPs  []net.IP
	mutex         sync.Mutex
	subnetMask    net.IPMask
	gateway       net.IP
}

// NewDHCPServer creates a new DHCP server instance
func NewDHCPServer(configFile string) (*DHCPServer, error) {
	configData, err := os.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, err
	}

	// Collect reserved IPs
	reservedIPs := make(map[string]struct{})
	for _, ip := range config.ReservedAddresses {
		reservedIPs[ip] = struct{}{}
	}

	// Initialize available IPs (192.168.2.2 - 192.168.2.254, excluding reserved)
	availableIPs := []net.IP{}
	for i := 2; i <= 254; i++ {
		ipStr := fmt.Sprintf("192.168.2.%d", i)
		if _, exists := reservedIPs[ipStr]; !exists {
			availableIPs = append(availableIPs, net.ParseIP(ipStr))
		}
	}

	return &DHCPServer{
		config:       config,
		leases:       make(map[string]*Lease),
		availableIPs: availableIPs,
		subnetMask:   net.CIDRMask(24, 32),
		gateway:      net.ParseIP("192.168.2.1"),
	}, nil
}

// getIPForClient gets an IP address for the client
func (s *DHCPServer) getIPForClient(mac net.HardwareAddr) (net.IP, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	macStr := mac.String()

	// Check for reserved IP
	if reservedIP, exists := s.config.ReservedAddresses[macStr]; exists {
		ip := net.ParseIP(reservedIP)
		if ip == nil {
			return nil, fmt.Errorf("invalid reserved IP for %s", macStr)
		}
		// Update or create lease for reserved IP
		if lease, exists := s.leases[macStr]; exists {
			lease.IP = ip
			lease.ExpiresAt = time.Now().Add(time.Duration(s.config.LeaseDuration) * time.Second)
		} else {
			s.leases[macStr] = &Lease{
				IP:        ip,
				MAC:       mac,
				ExpiresAt: time.Now().Add(time.Duration(s.config.LeaseDuration) * time.Second),
			}
		}
		return ip, nil
	}

	// Check for existing lease (even if expired)
	if lease, exists := s.leases[macStr]; exists {
		// If IP is still available (not reserved or assigned to another active lease), reuse it
		ip := lease.IP
		isAvailable := true
		for otherMac, otherLease := range s.leases {
			if otherMac != macStr && otherLease.IP.Equal(ip) && time.Now().Before(otherLease.ExpiresAt) {
				isAvailable = false
				break
			}
		}
		if isAvailable {
			lease.ExpiresAt = time.Now().Add(time.Duration(s.config.LeaseDuration) * time.Second)
			return ip, nil
		}
		// If IP is not available, remove the old lease and assign a new IP
		delete(s.leases, macStr)
	}

	// Clean up expired leases to reclaim IPs, but preserve lease records
	for mac, lease := range s.leases {
		if time.Now().After(lease.ExpiresAt) && mac != macStr {
			// Only add IP back if not reserved or actively used
			isReserved := false
			for _, reservedIP := range s.config.ReservedAddresses {
				if lease.IP.String() == reservedIP {
					isReserved = true
					break
				}
			}
			isUsed := false
			for otherMac, otherLease := range s.leases {
				if otherMac != mac && otherLease.IP.Equal(lease.IP) && time.Now().Before(otherLease.ExpiresAt) {
					isUsed = true
					break
				}
			}
			if !isReserved && !isUsed {
				s.availableIPs = append(s.availableIPs, lease.IP)
			}
			// Do not delete the lease to preserve history
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
		ExpiresAt: time.Now().Add(time.Duration(s.config.LeaseDuration) * time.Second),
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

		reply, err := dhcpv4.New(
			dhcpv4.WithReply(p),
			dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
			dhcpv4.WithYourIP(ip),
			dhcpv4.WithServerIP(s.gateway),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(s.subnetMask)),
			dhcpv4.WithOption(dhcpv4.OptRouter(s.gateway)),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Duration(s.config.LeaseDuration)*time.Second)),
		)
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

		reply, err := dhcpv4.New(
			dhcpv4.WithReply(p),
			dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
			dhcpv4.WithYourIP(ip),
			// dhcpv4.WithServerIP(s.gateway),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(s.subnetMask)),
			// dhcpv4.WithOption(dhcpv4.OptRouter(s.gateway)),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Duration(s.config.LeaseDuration)*time.Second)),
		)
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

func main() {
	// Define command-line flag for network interface
	iface := flag.String("iface", "en5", "Network interface to bind the DHCP server to")
	configFile := flag.String("config", "dhcp_config.yaml", "Path to the DHCP configuration file")
	flag.Parse()

	// Initialize DHCP server with config file
	server, err := NewDHCPServer(*configFile)
	if err != nil {
		log.Fatal(err)
	}

	// Set up UDP address for DHCP server
	addr := &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 67}
	s, err := server4.NewServer(* iface, addr, server.ServeDHCP)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Starting DHCP server on interface %s, port 67...", *iface)
	if err := s.Serve(); err != nil {
		log.Fatal(err)
	}
}

