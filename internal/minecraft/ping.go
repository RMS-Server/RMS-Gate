package minecraft

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// MCPing performs a Minecraft Server List Ping to check if server is fully started.
// This wrapper keeps the old signature but enforces a hard timeout over DNS + dial + status exchange.
func MCPing(addr net.Addr, timeout time.Duration) error {
	return MCPingAddrString(addr.String(), timeout)
}

// MCPingAddrString dials the address with a hard timeout and performs the status ping.
func MCPingAddrString(addr string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	return MCPingConn(conn, addr, timeout)
}

// MCPingConn performs the Minecraft status exchange on an already-established connection.
// The caller should set appropriate deadlines on conn.
func MCPingConn(conn net.Conn, addr string, timeout time.Duration) error {
	_ = conn.SetDeadline(time.Now().Add(timeout))

	host, portStr, _ := net.SplitHostPort(addr)
	var port uint16 = 25565
	fmt.Sscanf(portStr, "%d", &port)

	// Send handshake packet (packet ID 0x00)
	handshake := &bytes.Buffer{}
	writeVarInt(handshake, 0x00)                        // Packet ID
	writeVarInt(handshake, 767)                         // Protocol version (1.21)
	writeString(handshake, host)                        // Server address
	_ = binary.Write(handshake, binary.BigEndian, port) // Server port
	writeVarInt(handshake, 1)                           // Next state: Status

	if err := writePacket(conn, handshake.Bytes()); err != nil {
		return fmt.Errorf("failed to send handshake: %w", err)
	}

	// Send status request packet (packet ID 0x00, empty payload)
	statusReq := &bytes.Buffer{}
	writeVarInt(statusReq, 0x00) // Packet ID
	if err := writePacket(conn, statusReq.Bytes()); err != nil {
		return fmt.Errorf("failed to send status request: %w", err)
	}

	// Read status response
	_, packetData, err := readPacket(conn)
	if err != nil {
		return fmt.Errorf("failed to read status response: %w", err)
	}

	// Verify packet ID is 0x00 (status response)
	reader := bytes.NewReader(packetData)
	packetID, err := readVarInt(reader)
	if err != nil || packetID != 0x00 {
		return fmt.Errorf("unexpected packet ID: %d", packetID)
	}

	// Read JSON response string (we don't need to parse it, just verify it exists)
	jsonLen, err := readVarInt(reader)
	if err != nil || jsonLen <= 0 {
		return fmt.Errorf("invalid JSON response length: %d", jsonLen)
	}

	return nil
}

func writeVarInt(w io.Writer, value int32) {
	for {
		b := byte(value & 0x7F)
		value >>= 7
		if value != 0 {
			b |= 0x80
		}
		w.Write([]byte{b})
		if value == 0 {
			break
		}
	}
}

func writeString(w io.Writer, s string) {
	writeVarInt(w, int32(len(s)))
	w.Write([]byte(s))
}

func writePacket(conn net.Conn, data []byte) error {
	lenBuf := &bytes.Buffer{}
	writeVarInt(lenBuf, int32(len(data)))
	if _, err := conn.Write(lenBuf.Bytes()); err != nil {
		return err
	}
	if _, err := conn.Write(data); err != nil {
		return err
	}
	return nil
}

func readVarInt(r io.Reader) (int32, error) {
	var result int32
	var shift uint
	for {
		b := make([]byte, 1)
		if _, err := r.Read(b); err != nil {
			return 0, err
		}
		result |= int32(b[0]&0x7F) << shift
		if b[0]&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 32 {
			return 0, fmt.Errorf("VarInt too big")
		}
	}
	return result, nil
}

func readPacket(conn net.Conn) (int32, []byte, error) {
	length, err := readVarInt(conn)
	if err != nil {
		return 0, nil, err
	}
	if length <= 0 || length > 1024*1024 {
		return 0, nil, fmt.Errorf("invalid packet length: %d", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return 0, nil, err
	}
	return length, data, nil
}
