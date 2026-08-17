package ingest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// NativeServer — TCP/UDP listeners for the v1 ld_format protocol.
type NativeServer struct {
	app Appender

	tcpLn  net.Listener
	udpPC  net.PacketConn
	wg     sync.WaitGroup
	closed chan struct{}
}

// StartNative starts the listeners. An empty address disables the corresponding listener.
func StartNative(app Appender, tcpAddr, udpAddr string) (*NativeServer, error) {
	s := &NativeServer{app: app, closed: make(chan struct{})}

	if tcpAddr != "" {
		ln, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			return nil, err
		}
		s.tcpLn = ln
		s.wg.Add(1)
		go s.acceptLoop()
		slog.Info("native tcp listening", "addr", tcpAddr)
	}

	if udpAddr != "" {
		pc, err := net.ListenPacket("udp", udpAddr)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.udpPC = pc
		s.wg.Add(1)
		go s.udpLoop()
		slog.Info("native udp listening", "addr", udpAddr)
	}

	return s, nil
}

func (s *NativeServer) Close() {
	close(s.closed)
	if s.tcpLn != nil {
		_ = s.tcpLn.Close()
	}
	if s.udpPC != nil {
		_ = s.udpPC.Close()
	}
	s.wg.Wait()
}

func (s *NativeServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.tcpLn.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			slog.Warn("native tcp accept", "err", err)
			continue
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *NativeServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	remoteIP := hostOnly(conn.RemoteAddr())
	r := bufio.NewReaderSize(conn, 64<<10)

	err := ParseLDStream(r, func(ev ldEvent) {
		if e, ok := EntryFromLD(ev, remoteIP, time.Now()); ok {
			s.app.Append(e)
		}
	})
	if err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Warn("native tcp: parse failure, connection closed", "remote", remoteIP, "err", err)
	}
}

func (s *NativeServer) udpLoop() {
	defer s.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := s.udpPC.ReadFrom(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			slog.Warn("native udp read", "err", err)
			continue
		}
		remoteIP := hostOnly(addr)
		r := bufio.NewReader(bytes.NewReader(buf[:n]))
		if err := ParseLDStream(r, func(ev ldEvent) {
			if e, ok := EntryFromLD(ev, remoteIP, time.Now()); ok {
				s.app.Append(e)
			}
		}); err != nil {
			slog.Warn("native udp: datagram parse error", "remote", remoteIP, "err", err)
		}
	}
}

func hostOnly(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// Shutdown — Close that respects the context (for symmetry with http.Server).
func (s *NativeServer) Shutdown(ctx context.Context) {
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
