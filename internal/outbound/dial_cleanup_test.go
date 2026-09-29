package outbound

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

func TestOutboundProxyHandshakeCancellation(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()
	for _, protocol := range []string{"http", "socks4", "socks4a", "socks5"} {
		t.Run(protocol, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			peerResult := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					peerResult <- err
					return
				}
				defer conn.Close()
				// A silent proxy used to ignore cancellation until it closed the socket.
				_ = conn.SetReadDeadline(time.Now().Add(600 * time.Millisecond))
				_, err = io.Copy(io.Discard, conn)
				peerResult <- err
			}()
			opts := map[string]any{"type": "http", "server": "127.0.0.1", "server_port": ln.Addr().(*net.TCPAddr).Port}
			if strings.HasPrefix(protocol, "socks") {
				opts["type"], opts["version"] = "socks", strings.TrimPrefix(protocol, "socks")
			}
			raw, _ := json.Marshal(opts)
			ob, err := b.Build(raw)
			if err != nil {
				t.Fatal(err)
			}
			defer closeOutbound(ob)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			started := time.Now()
			conn, err := ob.DialContext(ctx, "tcp", M.ParseSocksaddr("192.0.2.1:443"))
			if conn != nil {
				conn.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("handshake error = %v, want context deadline", err)
			}
			if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
				t.Errorf("proxy handshake ignored cancellation: %s", elapsed)
			}
			if err := <-peerResult; err != nil {
				t.Errorf("proxy socket not closed at cancellation: %v", err)
			}
		})
	}
}

func TestOutboundProxyHandshakeSuccess(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()
	for _, protocol := range []string{"http", "socks"} {
		t.Run(protocol, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			peerResult := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					peerResult <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				if protocol == "http" {
					_, err = http.ReadRequest(bufio.NewReader(conn))
					if err == nil {
						_, err = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
					}
				} else {
					var greeting [3]byte
					_, err = io.ReadFull(conn, greeting[:])
					if err == nil {
						_, err = conn.Write([]byte{5, 0})
					}
					var request [10]byte // IPv4 CONNECT request.
					if err == nil {
						_, err = io.ReadFull(conn, request[:])
					}
					if err == nil {
						_, err = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1})
					}
				}
				if err == nil {
					var payload [4]byte
					_, err = io.ReadFull(conn, payload[:])
					if err == nil {
						_, err = conn.Write(payload[:])
					}
				}
				peerResult <- err
			}()
			raw := json.RawMessage(fmt.Sprintf(`{"type":%q,"server":"127.0.0.1","server_port":%d}`, protocol, ln.Addr().(*net.TCPAddr).Port))
			ob, err := b.Build(raw)
			if err != nil {
				t.Fatal(err)
			}
			defer closeOutbound(ob)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn, err := ob.DialContext(ctx, "tcp", M.ParseSocksaddr("192.0.2.1:443"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			cancel()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			var reply [4]byte
			if _, err := io.ReadFull(conn, reply[:]); err != nil || string(reply[:]) != "ping" {
				t.Fatalf("established proxy connection lost after dial cancellation: %q, %v", reply, err)
			}
			if err := <-peerResult; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A rejecting peer keeps its read side open, so the test observes whether the
// outbound closes its socket before returning an error, independently of GC.
func TestOutboundFailedHandshakeClosesSocket(t *testing.T) {
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	b := newTestSingboxBuilder(t)
	defer b.Close()
	for _, protocol := range []string{"vless", "vmess", "trojan"} {
		for _, mode := range []string{"tls", "tls-utls", "tls-reality", "tls-packet", "ws", "httpupgrade", "httpupgrade-timeout"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				peerResult := make(chan error, 1)
				go func() {
					conn, err := ln.Accept()
					if err != nil {
						peerResult <- err
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
					if strings.HasPrefix(mode, "tls") {
						var header [5]byte
						if _, err = io.ReadFull(conn, header[:]); err == nil {
							_, err = io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint16(header[3:])))
						}
						if err == nil {
							_, err = conn.Write([]byte{21, 3, 3, 0, 2, 2, 40})
						}
					} else {
						var p [4096]byte
						_, err = conn.Read(p[:])
						if err == nil && mode != "httpupgrade-timeout" {
							_, err = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
						}
					}
					if err != nil {
						peerResult <- err
						return
					}
					if mode != "httpupgrade-timeout" {
						_ = conn.(*net.TCPConn).CloseWrite()
					}
					_ = conn.SetReadDeadline(time.Now().Add(time.Second))
					_, err = io.Copy(io.Discard, conn)
					peerResult <- err
				}()
				opts := map[string]any{"type": protocol, "server": "127.0.0.1", "server_port": ln.Addr().(*net.TCPAddr).Port,
					"uuid": "00000000-0000-4000-8000-000000000001"}
				if protocol == "trojan" {
					delete(opts, "uuid")
					opts["password"] = "test-only"
				}
				if strings.HasPrefix(mode, "tls") {
					tlsOpts := map[string]any{"enabled": true, "insecure": true}
					if mode == "tls-utls" || mode == "tls-reality" {
						tlsOpts["utls"] = map[string]any{"enabled": true, "fingerprint": "chrome"}
					}
					if mode == "tls-reality" {
						tlsOpts["insecure"] = false
						tlsOpts["server_name"] = "example.com"
						tlsOpts["reality"] = map[string]any{"enabled": true, "public_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAk", "short_id": "01"}
					}
					opts["tls"] = tlsOpts
				} else {
					opts["transport"] = map[string]any{"type": strings.TrimSuffix(mode, "-timeout"), "path": "/"}
				}
				raw, _ := json.Marshal(opts)
				ob, err := b.Build(raw)
				if err != nil {
					if strings.Contains(err.Error(), "not included") {
						t.Skip(err)
					}
					t.Fatal(err)
				}
				defer closeOutbound(ob)
				timeout := 2 * time.Second
				if mode == "httpupgrade-timeout" {
					timeout = 200 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				var conn io.Closer
				if mode == "tls-packet" {
					packet, dialErr := ob.ListenPacket(ctx, M.ParseSocksaddr("example.com:53"))
					err = dialErr
					if packet != nil {
						conn = packet
					}
				} else {
					stream, dialErr := ob.DialContext(ctx, "tcp", M.ParseSocksaddr("example.com:443"))
					err = dialErr
					if stream != nil {
						conn = stream
					}
				}
				if conn != nil {
					conn.Close()
				}
				if err == nil {
					t.Fatal("expected handshake rejection")
				}
				if peerErr := <-peerResult; peerErr != nil {
					t.Fatalf("socket remained open after rejected handshake: %v", peerErr)
				}
			})
		}
	}
}

func TestOutboundSuccessfulTLSRetainsSocket(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()
	fixture := httptest.NewTLSServer(nil)
	tlsConfig := fixture.TLS.Clone()
	fixture.Close()
	for _, protocol := range []string{"vless", "vmess", "trojan"} {
		t.Run(protocol, func(t *testing.T) {
			ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			readResult := make(chan error, 1)
			go func() {
				peer, err := ln.Accept()
				if err != nil {
					readResult <- err
					return
				}
				defer peer.Close()
				_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
				var data [1]byte
				_, err = io.ReadFull(peer, data[:])
				readResult <- err
			}()
			opts := map[string]any{"type": protocol, "server": "127.0.0.1", "server_port": ln.Addr().(*net.TCPAddr).Port,
				"uuid": "00000000-0000-4000-8000-000000000001", "tls": map[string]any{"enabled": true, "insecure": true}}
			if protocol == "trojan" {
				delete(opts, "uuid")
				opts["password"] = "test-only"
			}
			raw, _ := json.Marshal(opts)
			ob, err := b.Build(raw)
			if err != nil {
				t.Fatal(err)
			}
			defer closeOutbound(ob)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn, err := ob.DialContext(ctx, "tcp", M.ParseSocksaddr("example.com:443"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			cancel() // Dial cancellation must not own an established connection.
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
			if err := <-readResult; err != nil {
				t.Fatalf("successful socket closed too soon: %v", err)
			}
		})
	}
}

func TestOutboundDialCleanupKeepsNodeIsolation(t *testing.T) {
	b := newTestSingboxBuilder(t)
	t.Cleanup(func() { _ = b.Close() })
	var outbounds []adapter.Outbound
	for i := 0; i < 2; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func(marker byte) {
			peer, err := ln.Accept()
			if err != nil {
				return
			}
			defer peer.Close()
			_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
			var data [4096]byte
			if _, err := peer.Read(data[:]); err == nil {
				_, _ = peer.Write([]byte{0, 0, marker})
			}
		}(byte(i))
		raw := json.RawMessage(fmt.Sprintf(`{"type":"vless","server":"127.0.0.1","server_port":%d,"uuid":"00000000-0000-4000-8000-000000000001"}`, ln.Addr().(*net.TCPAddr).Port))
		ob, err := b.Build(raw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeOutbound(ob) })
		outbounds = append(outbounds, ob)
	}
	if service.FromContext[adapter.OutboundManager](b.ctx) != b.outboundManager {
		t.Fatal("node build changed the shared outbound manager")
	}
	if _, exists := b.outboundManager.Outbound(ownedDialerTag); exists {
		t.Fatal("node-local dialer leaked into the shared manager")
	}
	for i, ob := range outbounds {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, err := ob.DialContext(ctx, "tcp", M.ParseSocksaddr("example.com:443"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
			var reply [1]byte
			if _, err := io.ReadFull(conn, reply[:]); err != nil {
				t.Fatal(err)
			}
			if reply[0] != byte(i) {
				t.Fatalf("dialed node %d, want %d", reply[0], i)
			}
		})
	}
}
