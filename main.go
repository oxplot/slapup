package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type connMap struct {
	localHost  string
	localPort  int
	remoteHost string
	remotePort int
}

func (m connMap) String() string {
	return fmt.Sprintf("%s:%d:%s:%d", m.localHost, m.localPort, m.remoteHost, m.remotePort)
}

type connMapFlag []connMap

func (i connMapFlag) String() string {
	strs := make([]string, len(i))
	for idx, m := range i {
		strs[idx] = m.String()
	}
	return strings.Join(strs, ",")
}

func (i *connMapFlag) Set(value string) error {
	parts := strings.SplitN(value, ":", 5)
	var localHost, remoteHost string
	var localPort, remotePort int
	switch len(parts) {
	case 2:
		localHost = "127.0.0.1"
		localPort, _ = strconv.Atoi(parts[0])
		remoteHost = "127.0.0.1"
		remotePort, _ = strconv.Atoi(parts[1])
	case 3:
		localHost = "127.0.0.1"
		localPort, _ = strconv.Atoi(parts[0])
		remoteHost = parts[1]
		remotePort, _ = strconv.Atoi(parts[2])
	case 4:
		localHost = parts[0]
		localPort, _ = strconv.Atoi(parts[1])
		remoteHost = parts[2]
		remotePort, _ = strconv.Atoi(parts[3])
	default:
		return fmt.Errorf("Invalid connection map: %s", value)
	}
	if localPort == 0 || remotePort == 0 {
		return fmt.Errorf("Invalid connection map: %s", value)
	}
	m := connMap{
		localHost:  localHost,
		localPort:  localPort,
		remoteHost: remoteHost,
		remotePort: remotePort,
	}
	*i = append(*i, m)
	return nil
}

func handleConnection(src net.Conn, dst net.Conn) {
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer src.Close()
		defer dst.Close()
		io.Copy(src, dst)
	}()
	go func() {
		defer wg.Done()
		defer src.Close()
		defer dst.Close()
		io.Copy(dst, src)
	}()
	wg.Wait()
}

func main() {
	defer os.Exit(1)

	var cm connMapFlag
	flag.Var(&cm, "l", "localhost:port:remotehost:port")
	onConn := flag.String("on-conn", "", "command to run on connection in shell - %h and %p will be replaced with host and port")
	flag.Parse()
	cmd := flag.Args()
	if len(cm) == 0 {
		slog.Error("No connections specified")
		return
	}
	if len(cmd) == 0 {
		slog.Error("No command specified")
		return
	}

	fatalErr := make(chan error)

	type connSpec struct {
		src net.Conn
		dst string
	}
	connSpecs := make(chan connSpec, 1)
	procEnable := make(chan bool, 1)

	go func() {
		never := time.Hour * 1000000
		enabled := false
		var proc *exec.Cmd
		restart := make(chan struct{}, 1)
		stopTimer := time.NewTimer(never)
		for {
			select {

			case <-restart:
				if proc != nil {
					slog.Info("Stopping process")
					proc.Process.Signal(syscall.SIGTERM)
					proc.Wait()
					slog.Info("Stopped process")
					proc = nil
				}
				if enabled {
					slog.Info("Starting process")
					proc = exec.Command(cmd[0], cmd[1:]...)
					proc.WaitDelay = 2 * time.Second
					if err := proc.Start(); err != nil {
						fatalErr <- fmt.Errorf("Could not start process: %v", err)
						return
					}
					go func() {
						proc.Wait()
						restart <- struct{}{}
					}()
				}

			case e := <-procEnable:
				if e {
					if !stopTimer.Stop() {
						<-stopTimer.C
					}
					stopTimer.Reset(never)
					if !enabled {
						enabled = true
						restart <- struct{}{}
					}
				}
				if !e && enabled {
					if !stopTimer.Stop() {
						<-stopTimer.C
					}
					stopTimer.Reset(10 * time.Second)
				}

			case <-stopTimer.C:
				enabled = false
				restart <- struct{}{}
				stopTimer.Reset(never)
			}

		}
	}()

	go func() {
		connected := 0
		done := make(chan struct{}, 1)
		for {
			select {
			case spec := <-connSpecs:
				go func() {
					defer func() {
						spec.src.Close()
						done <- struct{}{}
					}()
					c := *onConn
					if c != "" {
						addr := spec.src.LocalAddr().(*net.TCPAddr)
						c = strings.Replace(c, "%p", strconv.Itoa(addr.Port), -1)
						c = strings.Replace(c, "%h", addr.IP.String(), -1)
						cmd := exec.Command("/bin/sh", "-c", c)
						if err := cmd.Run(); err != nil {
							slog.Info("on-conn command failed", "err", err)
							return
						}
					}
					procEnable <- true
					var dst net.Conn
					s := time.Now()
					for time.Since(s) < 10*time.Second {
						conn, err := net.Dial("tcp", spec.dst)
						if err == nil {
							dst = conn
							break
						}
						slog.Info("Could not connect", "err", err)
						time.Sleep(100 * time.Millisecond)
					}
					if dst == nil {
						slog.Info("Could not connect to destination")
						return
					}
					defer dst.Close()
					handleConnection(spec.src, dst)
				}()
				connected++
				slog.Info("Connected", "count", connected)
			case <-done:
				connected--
				slog.Info("Disconnected", "count", connected)
				if connected == 0 {
					procEnable <- false
				}
			}
		}
	}()

	// Listen on all ports per config.

	for _, m := range cm {
		go func(m connMap) {
			listen, err := net.Listen("tcp", fmt.Sprintf("%s:%d", m.localHost, m.localPort))
			if err != nil {
				fatalErr <- fmt.Errorf("Could not listen on %s:%d: %v", m.localHost, m.localPort, err)
				return
			}
			for {
				src, err := listen.Accept()
				if err != nil {
					slog.Info("Could not accept connection", "err", err)
					continue
				}
				connSpecs <- connSpec{
					src: src,
					dst: fmt.Sprintf("%s:%d", m.remoteHost, m.remotePort),
				}
			}
		}(m)
	}

	sigCh := make(chan os.Signal)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fatalErr <- fmt.Errorf("Interrupted")
	}()

	slog.Error("error", "err", <-fatalErr)
}
