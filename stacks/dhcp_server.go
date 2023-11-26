package stacks

import (
	"encoding/binary"
	"errors"
	"net/netip"

	"github.com/soypat/seqs/eth"
)

type dhcpclient struct {
	addr        netip.Addr
	state       uint8
	port        uint16
	requestlist [10]byte
}

type dhcpOption struct {
	Opt  eth.DHCPOption
	Data []byte
}

type DHCPServer struct {
	mac      [6]byte
	nextAddr netip.Addr
	siaddr   netip.Addr
	port     uint16
	hosts    map[[6]byte]dhcpclient
}

func NewDHCPServer(port uint16, mac [6]byte, siaddr netip.Addr) *DHCPServer {
	return &DHCPServer{
		port:   port,
		mac:    mac,
		siaddr: siaddr,
		hosts:  make(map[[6]byte]dhcpclient),
	}
}

func parseDHCP(incpayload []byte, fn func(opt dhcpOption) error) (eth.DHCPHeader, error) {
	const (
		sizeSName     = 64  // Server name, part of BOOTP too.
		sizeFILE      = 128 // Boot file name, Legacy.
		sizeOptions   = 312
		dhcpOffset    = eth.SizeEthernetHeader + eth.SizeIPv4Header + eth.SizeUDPHeader
		optionsStart  = dhcpOffset + eth.SizeDHCPHeader + sizeSName + sizeFILE
		sizeDHCPTotal = eth.SizeDHCPHeader + sizeSName + sizeFILE + sizeOptions
	)
	var hdr eth.DHCPHeader
	if len(incpayload) < eth.SizeDHCPHeader {
		return hdr, errors.New("short payload to parse DHCP")
	} else if fn == nil {
		return hdr, errors.New("nil function to parse DHCP")
	}

	hdr = eth.DecodeDHCPHeader(incpayload)
	// Parse DHCP options.
	ptr := eth.SizeDHCPHeader + sizeSName + sizeFILE + 4
	if ptr >= len(incpayload) {
		return hdr, errors.New("short payload to parse DHCP options")
	}
	for ptr+1 < len(incpayload) && int(incpayload[ptr+1]) < len(incpayload) {
		if incpayload[ptr] == 0xff {
			break
		}
		option := eth.DHCPOption(incpayload[ptr])
		optlen := incpayload[ptr+1]
		optionData := incpayload[ptr+2 : ptr+2+int(optlen)]
		if err := fn(dhcpOption{option, optionData}); err != nil {
			return hdr, err
		}
		ptr += int(optlen) + 2
	}
	return hdr, nil
}

func (d *DHCPServer) HandleUDP(resp []byte, packet *UDPPacket) (_ int, err error) {
	const (
		sizeSName     = 64  // Server name, part of BOOTP too.
		sizeFILE      = 128 // Boot file name, Legacy.
		sizeOptions   = 312
		dhcpOffset    = eth.SizeEthernetHeader + eth.SizeIPv4Header + eth.SizeUDPHeader
		optionsStart  = dhcpOffset + eth.SizeDHCPHeader + sizeSName + sizeFILE
		sizeDHCPTotal = eth.SizeDHCPHeader + sizeSName + sizeFILE + sizeOptions
	)
	// First action is used to send data without having received a packet
	// so hasPacket will be false.
	hasPacket := packet.HasPacket()
	incpayload := packet.Payload()
	switch {
	case len(resp) < sizeDHCPTotal:
		return 0, errors.New("short payload to marshall DHCP")
	case hasPacket && len(incpayload) < eth.SizeDHCPHeader:
		return 0, errors.New("short payload to parse DHCP")
	}

	var rcvHdr eth.DHCPHeader
	if !hasPacket {
		return 0, nil
	}

	mac := packet.Eth.Source
	client := d.hosts[mac]
	var msgType uint8
	rcvHdr, err = parseDHCP(incpayload, func(opt dhcpOption) error {
		switch opt.Opt {
		case eth.DHCP_MessageType:
			if len(opt.Data) == 1 {
				msgType = opt.Data[0]
			}
		case eth.DHCP_ParameterRequestList:
			client.requestlist = [10]byte{}
			copy(client.requestlist[:], opt.Data)
		case eth.DHCP_RequestedIPaddress:
			if len(opt.Data) == 4 && client.state == dhcpStateNone {
				client.addr = netip.AddrFrom4([4]byte(opt.Data))
			}
		}
		return nil
	})
	if err != nil || (msgType != 1 && rcvHdr.SIAddr != d.siaddr.As4()) {
		return 0, err
	}

	var Options []dhcpOption
	switch msgType {
	case 1: // DHCP Discover.
		if client.state != dhcpStateNone {
			err = errors.New("DHCP Discover on initialized client")
			break
		}
		rcvHdr.YIAddr = d.next(client.addr.As4())
		Options = []dhcpOption{
			{eth.DHCP_MessageType, []byte{2}}, // DHCP Message Type: Offer
		}
		rcvHdr.SIAddr = d.siaddr.As4()
		client.port = packet.UDP.SourcePort
		client.state = dhcpStateWaitOffer

	case 3: // DHCP Request.
		if client.state != dhcpStateWaitOffer {
			err = errors.New("unexpected DHCP Request")
			break
		}
		Options = []dhcpOption{
			{eth.DHCP_MessageType, []byte{5}}, // DHCP Message Type: ACK
		}
	}
	if err != nil {
		return 0, nil
	}
	d.hosts[mac] = client
	for i := dhcpOffset + 14; i < len(resp); i++ {
		resp[i] = 0 // Zero out BOOTP and options fields.
	}
	rcvHdr.Put(resp[dhcpOffset:])
	// Encode DHCP header + options.
	const magicCookie = 0x63825363
	ptr := optionsStart
	binary.BigEndian.PutUint32(resp[ptr:], magicCookie)
	ptr += 4
	for _, opt := range Options {
		ptr += encodeDHCPOption(resp[ptr:], opt)
	}
	resp[ptr] = 0xff // endmark
	// Set Ethernet+IP+UDP headers.
	payload := resp[dhcpOffset : dhcpOffset+sizeDHCPTotal]
	d.setResponseUDP(client.port, packet, payload)
	packet.PutHeaders(resp)
	return dhcpOffset + sizeDHCPTotal, nil
}

func (d *DHCPServer) next(requested [4]byte) [4]byte {
	if requested != [4]byte{} {
		return requested
	}
	return [4]byte{192, 168, 1, 2}
}

func (d *DHCPServer) setResponseUDP(clientport uint16, packet *UDPPacket, payload []byte) {
	const ipLenInWords = 5
	// Ethernet frame.
	packet.Eth.Destination = eth.BroadcastHW6()
	copy(packet.Eth.Source[:], d.mac[:])
	packet.Eth.SizeOrEtherType = uint16(eth.EtherTypeIPv4)

	// IPv4 frame.
	packet.IP.Destination = [4]byte{}
	packet.IP.Source = d.siaddr.As4() // Source IP is always zeroed when client sends.
	packet.IP.Protocol = 17           // UDP
	packet.IP.TTL = 64
	packet.IP.ID = prand16(packet.IP.ID)
	packet.IP.VersionAndIHL = ipLenInWords // Sets IHL: No IP options. Version set automatically.
	packet.IP.TotalLength = 4*ipLenInWords + eth.SizeUDPHeader + uint16(len(payload))
	packet.IP.Checksum = packet.IP.CalculateChecksum()
	// TODO(soypat): Document why disabling ToS used by DHCP server may cause Request to fail.
	// Apparently server sets ToS=192. Uncommenting this line causes DHCP to fail on my setup.
	// If left fixed at 192, DHCP does not work.
	// If left fixed at 0, DHCP does not work.
	// Apparently ToS is a function of which state of DHCP one is in. Not sure why code below works.
	packet.IP.ToS = 192
	packet.IP.Flags = 0

	// UDP frame.
	packet.UDP.DestinationPort = clientport
	packet.UDP.SourcePort = d.port
	packet.UDP.Length = packet.IP.TotalLength - 4*ipLenInWords
	packet.UDP.Checksum = packet.UDP.CalculateChecksumIPv4(&packet.IP, payload)
}