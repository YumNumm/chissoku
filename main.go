// Package chissoku implements main chissoku program
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alecthomas/kong"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"

	"github.com/northeye/chissoku/options"
	"github.com/northeye/chissoku/output"
	"github.com/northeye/chissoku/types"
)

func main() {
	var c Chissoku
	ctx := kong.Parse(&c,
		kong.Name(ProgramName),
		kong.Vars{"version": "v" + Version, "outputters": strings.Join(c.registerOutputters(), ",")},
		kong.Description(Description),
		kong.Bind(&c.Options))
	if err := ctx.Run(); err != nil {
		slog.Error("chissoku.Run()", "error", err)
		os.Exit(1)
	}
}

// Chissoku main program
type Chissoku struct {
	// Options
	Options options.Options `embed:""`

	// Stdout output
	output.Stdout `prefix:"stdout." group:"Stdout Output:"`
	// MQTT output
	output.Mqtt `prefix:"mqtt." group:"MQTT Output:"`
	// Prometheus output
	output.Prometheus `prefix:"prometheus." group:"Prometheus Output:"`

	// available outputters
	outputters map[string]output.Outputter
	// active outputters
	activeOutputters atomic.Value

	// reader channel
	rchan chan *types.Data
	// deactivate outputter
	dechan chan string
	// cancel
	cancel func()

	// serial device
	port serial.Port
	// serial scanner
	scanner *bufio.Scanner

	// cleanup
	cleanup func()
}

// AfterApply kong hook
func (c *Chissoku) AfterApply(opts *options.Options) error {
	var writer io.Writer = os.Stderr
	level := slog.LevelInfo
	if opts.Debug {
		level = slog.LevelDebug
	}
	if opts.Quiet {
		writer = io.Discard
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level})))

	c.rchan = make(chan *types.Data)
	c.dechan = make(chan string)
	ctx := output.ContextWithDeactivateChannel(context.Background(), c.dechan)
	ctx = context.WithValue(ctx, options.ContextKeyOptions{}, opts)
	ctx, c.cancel = context.WithCancel(ctx)

	// initialize and filter outputters
	a := make(map[string]output.Outputter, len(opts.Output))
	for _, name := range opts.Output {
		if o, ok := c.outputters[name]; ok {
			if err := o.Initialize(ctx); err != nil {
				slog.Error("Initialize outputter", "outputter", o.Name(), "error", err)
				continue
			}
			a[name] = o
		}
	}
	if len(a) == 0 {
		return fmt.Errorf("no active outputters are avaiable")
	}
	c.activeOutputters.Store(a)

	c.cleanup = sync.OnceFunc(func() {
		// exit the program cleanly
		c.cancel()
		for _, o := range c.activeOutputters.Load().(map[string]output.Outputter) {
			o.Close()
		}
		if c.port != nil {
			slog.Debug("Sending command", "command", CommandSTP)
			// nolint: errcheck
			c.port.Write([]byte(CommandSTP + "\r\n"))
		}
	})

	return nil
}

const (
	// CommandSTP the STP Command
	CommandSTP string = `STP`
	// CommandID the ID? Command
	CommandID string = `ID?`
	// CommandSTA the STA Command
	CommandSTA string = `STA`
	// ResponseOK the OK response
	ResponseOK string = `OK`
	// ResponseNG the NG response
	ResponseNG string = `NG`
)

func (c *Chissoku) findUDCO2S() (port string, err error) {
	ports, err :=enumerator.GetDetailedPortsList()
	if err != nil {
		slog.Error("Getting serial ports", "error", err)
		return "", err
	}

	for _, port := range ports {
		if port.IsUSB && port.PID == "E95A" && port.VID == "04D8" {
			slog.Debug("Found UD-CO2S", "port", port)
			return port.Name, nil
		}
	}
	return "", fmt.Errorf("UD-CO2S not found")
}

// Run run the program
func (c *Chissoku) Run() (err error) {
	slog.Debug("Start", "name", ProgramName, "version", Version)

	port, err := c.findUDCO2S()
	if err != nil {
		slog.Error("Finding UD-CO2S", "error", err)
		return err
	}
	slog.Info("Found UD-CO2S", "port", port)

	if c.port, err = serial.Open(port, &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		StopBits: serial.OneStopBit,
		Parity:   serial.NoParity,
	}); err != nil {
		slog.Error("Opening serial", "error", err, "device", port)
		return err
	}
	c.port.SetReadTimeout(time.Second * 10)

	// initialize UD-CO2S
	if err := c.prepareDevice(); err != nil {
		return err
	}

	// signalHandler
	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, os.Interrupt)
	// signal handler
	go func() {
		<-sigch
		c.cleanup()
		<-time.After(time.Second)
		slog.Error("No response from device")
		os.Exit(128)
	}()

	// main
	go c.dispatch()

	if err = c.readDevice(); err != nil {
		slog.Error("Error on readDevice", "err", err)
	}
	slog.Debug("Close Serial port")
	c.port.Close()

	return err
}

// readDevice read data from serial device
func (c *Chissoku) readDevice() error {
	re := regexp.MustCompile(`CO2=(\d+),HUM=([0-9\.]+),TMP=([0-9\.-]+)`)
	// as main loop
	for c.scanner.Scan() {
		text := c.scanner.Text()
		m := re.FindAllStringSubmatch(text, -1)
		if len(m) > 0 {
			d := &types.Data{Timestamp: types.ISO8601Time(time.Now()), Tags: c.Options.Tags}
			d.CO2, _ = strconv.ParseInt(m[0][1], 10, 64)
			d.Humidity, _ = strconv.ParseFloat(m[0][2], 64)
			d.Temperature, _ = strconv.ParseFloat(m[0][3], 64)
			c.rchan <- d
		} else if text[:6] == `OK STP` {
			return nil // exit 0
		} else {
			slog.Warn("Read unmatched string", "str", text)
		}
	}
	if err := c.scanner.Err(); err != nil {
		slog.Error("Scanner read error", "error", err)
		c.cleanup()
		return err
	}
	return nil
}

func (c *Chissoku) dispatch() {
	for {
		select {
		case deactivate := <-c.dechan:
			a := c.activeOutputters.Load().(map[string]output.Outputter)
			delete(a, deactivate)
			if len(a) == 0 {
				slog.Debug("No outputers are alive")
				c.cleanup()
				return
			}
			c.activeOutputters.Store(a)
		case data, more := <-c.rchan:
			if !more {
				slog.Debug("Reader channel has ben closed")
				return
			}
			for _, o := range c.activeOutputters.Load().(map[string]output.Outputter) {
				o.Output(data)
			}
		}
	}
}

// initialize and prepare the device
func (c *Chissoku) prepareDevice() (err error) {
	c.scanner = bufio.NewScanner(c.port)
	c.scanner.Split(bufio.ScanLines)

	commands := []string{CommandSTP, CommandID, CommandSTA}
	do := make([]string, 0, len(commands))
	defer func() {
		level := slog.LevelInfo
		if err != nil {
			level = slog.LevelError
		}
		slog.Log(context.Background(), level, "Prepare UD-CO2S", "commands", do, "error", err)
	}()
	for _, cmd := range commands {
		do = append(do, cmd)
		if _, err = c.port.Write([]byte(cmd + "\r\n")); err != nil {
			return
		}
		time.Sleep(time.Millisecond * 100) // wait
		for c.scanner.Scan() {
			t := c.scanner.Text()
			if strings.HasPrefix(t[:2], ResponseOK) {
				break
			} else if strings.HasPrefix(t[:2], ResponseNG) {
				return fmt.Errorf("command `%v` failed", cmd)
			}
		}
	}
	return
}

// OutputterNames returns names of impleneted outputter
func (c *Chissoku) registerOutputters() (names []string) {
	if c.outputters != nil {
		for k := range c.outputters {
			names = append(names, k)
		}
		return names
	}
	c.outputters = make(map[string]output.Outputter)
	rv := reflect.Indirect(reflect.ValueOf(c))
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if value, ok := rv.Field(i).Addr().Interface().(output.Outputter); ok {
			name := value.Name()
			names = append(names, name)
			c.outputters[name] = value
		}
	}
	return names
}
