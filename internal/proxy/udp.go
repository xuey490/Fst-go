package proxy

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

func writeDatagram(w io.Writer, p []byte) error {
	if len(p) > 65535 {
		p = p[:65535]
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(p)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(p)
	return err
}

func readDatagram(r io.Reader, buf []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > len(buf) {
		tmp := make([]byte, n)
		if _, err := io.ReadFull(r, tmp); err != nil {
			return 0, err
		}
		return copy(buf, tmp), nil
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

func pipeUDP(stream io.ReadWriteCloser, pc net.PacketConn, remote net.Addr) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			n, err := readDatagram(stream, buf)
			if err != nil {
				_ = pc.SetDeadline(time.Now())
				return
			}
			if _, err := pc.WriteTo(buf[:n], remote); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				_ = stream.Close()
				return
			}
			if err := writeDatagram(stream, buf[:n]); err != nil {
				return
			}
		}
	}()
	wg.Wait()
}

type streamPacketConn struct {
	io.ReadWriteCloser
	remote net.Addr
	local  net.Addr
}

func (c *streamPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := readDatagram(c.ReadWriteCloser, p)
	return n, c.remote, err
}

func (c *streamPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	if err := writeDatagram(c.ReadWriteCloser, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *streamPacketConn) LocalAddr() net.Addr { return c.local }
func (c *streamPacketConn) SetDeadline(t time.Time) error {
	return setStreamDeadline(c.ReadWriteCloser, t)
}
func (c *streamPacketConn) SetReadDeadline(t time.Time) error {
	return setStreamDeadline(c.ReadWriteCloser, t)
}
func (c *streamPacketConn) SetWriteDeadline(t time.Time) error {
	return setStreamDeadline(c.ReadWriteCloser, t)
}

func setStreamDeadline(c io.ReadWriteCloser, t time.Time) error {
	type deadliner interface{ SetDeadline(time.Time) error }
	if d, ok := c.(deadliner); ok {
		return d.SetDeadline(t)
	}
	return nil
}
