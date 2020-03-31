package flags

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/igolaizola/tutu"
)

// TODO(igolaizola)
// -l, --listen                   Bind and listen for incoming connections
// -w, --wait <time>              Connect timeout
// -k, --keep                     Accept multiple connections in listen mode
// -i, --input                    Only read data from stream
// -o, --output                   Only write data to stream
var usage = `Usage of tutu:
    tutu [global_options] [[stream_options]...]

Global options:
    -v, --verbose                  Enable verbosity
    -c, --config                   Config file
    -h, --help                     Display this help screen
        --version                  Display version

Stream options:
    -<N> <type>     N is an integer value specifying the stream number
	
    If 0, this stream output can't be used by other streams
    Each stream option list must start with this option
    Available types: [stdio, file, cmd, tcp, udp, http, ws, mqtt, null]
	
    -i, --input <number>           Receive input only from specified stream numbers (N > 0)
    -c, --auto-close               Close stream when there is no more input data
    -w, --wait                     Connect timeout
    -e, --exec <command>           Executes the given command
    -a, --arg <arg>                Args for the command execution
    -x, --hex                      Parse data as hex
    -n, --eol                      Add end of line after every data written
    -f, --file <filename>          Filename to be used as stream (type: file)
    -r, --remote <host[:port]|url> Specify remote address and port or url to use   
    -s, --source <host:port>       Specify source address and port to use
    -k, --insecure                 Skip certificate validations 
	-h, --header <header:value>    Custom http headers
        --method                   HTTP method (http)
        --proto                    Custom protocol (ws)
        --sub                      Topic to subscribe (mqtt)
        --pub                      Topic to publish (mqtt)
        --dtls server|client       Apply DTLS layer (server/client)
		--dtls-state               Use serialized state of a previous dtls connection

`

// Parse parses commandline and config file arguments
func Parse(ver string) ([]*tutu.Config, bool, error) {
	// Global flags
	var verbose bool
	var version bool
	fs := flag.NewFlagSet("TUTU", flag.ExitOnError)
	fs.Usage = func() { _, _ = fmt.Fprint(fs.Output(), usage) }
	fs.BoolVar(&verbose, "v", false, "")
	fs.BoolVar(&verbose, "verbose", false, "")
	fs.BoolVar(&version, "version", false, "")
	var config string
	fs.StringVar(&config, "config", "", "")

	// Split into global, left and right
	global, streams := splitArgs(os.Args[1:])

	// Parse global args and config file
	if err := fs.Parse(global); err != nil {
		return nil, false, fmt.Errorf("couldn't parse global args and config file: %w", err)
	}
	var cfgArgs []string
	if len(config) > 0 {
		var err error
		cfgArgs, err = fromConfig(config)
		if err != nil {
			return nil, false, fmt.Errorf("couldn't parse config file: %w", err)

		}
	}

	// Split args from config file
	globalCfg, streamCfgs := splitArgs(cfgArgs)

	// Parse global flags from config file
	if err := fs.Parse(globalCfg); err != nil {
		return nil, false, fmt.Errorf("couldn't parse global args from config file: %w", err)
	}

	// Print version if requested
	if version {
		return nil, false, errors.New(ver)
	}

	// Merge stream args with config args
	streams = append(streams, streamCfgs...)
	if len(streams) == 0 {
		return nil, false, fmt.Errorf("at least 1 stream must be configured")
	}

	// Parse stream flags
	var configs []*tutu.Config
	for _, s := range streams {
		cfg, err := fromFlags(s)
		if err != nil {
			return nil, false, fmt.Errorf("couldn't parse streams args: %w", err)
		}
		configs = append(configs, cfg)
	}
	return configs, verbose, nil
}

// splitArgs split args into global and stream flags
func splitArgs(args []string) ([]string, [][]string) {
	var pre []string
	var streams [][]string
	lastIndex := -1
	for i := 0; i <= len(args); i++ {
		groupFound := false
		arg := ""
		if i < len(args) {
			arg = args[i]
		}

		// search group-type argument
		numArg := strings.SplitN(strings.TrimPrefix(arg, "-"), "=", 2)[0]
		if _, err := strconv.Atoi(numArg); err == nil && arg[0] == '-' {
			groupFound = true
		}

		if i < len(args) && !groupFound {
			continue
		}

		switch {
		case lastIndex < 0:
			pre = args[0:i]
		default:
			streams = append(streams, args[lastIndex:i])
		}
		lastIndex = i
	}
	return pre, streams
}

func fromConfig(config string) ([]string, error) {
	var args []string
	file, err := os.Open(config)
	if err != nil {
		return nil, fmt.Errorf("couldn't open config file %s", config)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.SplitN(line, " #", 2)[0]
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		split := strings.Split(line, " ")
		switch len(split) {
		case 1:
			args = append(args, fmt.Sprintf("-%s", split[0]))
		case 2:
			args = append(args, fmt.Sprintf("-%s=%s", split[0], split[1]))
		default:
			args = append(args, fmt.Sprintf("-%s", split[0]))
			args = append(args, split[1:]...)
		}
	}
	return args, nil
}

func fromFlags(args []string) (*tutu.Config, error) {
	cfg := &tutu.Config{}

	flags := flag.NewFlagSet(args[0], flag.ExitOnError)
	flags.Usage = func() { _, _ = fmt.Fprint(flags.Output(), usage) }
	fs := flagSet{FlagSet: flags}

	// Number and Type params
	numArg := strings.SplitN(strings.TrimPrefix(args[0], "-"), "=", 2)[0]
	number, err := strconv.Atoi(numArg)
	if err != nil {
		return nil, fmt.Errorf("couldn't parse stream number")
	}
	cfg.Number = number
	fs.stringVar(&cfg.Type, numArg, "", "")

	// Command params
	fs.stringVar(&cfg.Exec, "exec", "e", "")
	argsVal := &stringsValue{strings: &cfg.Args}
	fs.customVar(argsVal, "arg", "a")

	// Parsing
	fs.boolVar(&cfg.Hex, "hex", "x", false)
	fs.boolVar(&cfg.EOL, "eol", "n", false)

	// File/stdio
	fs.stringVar(&cfg.File, "file", "f", "")

	// Network params
	fs.stringVar(&cfg.Remote, "remote", "r", "")
	fs.stringVar(&cfg.Source, "source", "s", "")
	fs.boolVar(&cfg.Insecure, "insecure", "k", false)
	fs.durationVar(&cfg.Wait, "wait", "w", 30*time.Second)

	// WS params
	fs.stringVar(&cfg.Protocol, "proto", "", "")

	// MQTT params
	fs.stringVar(&cfg.Pub, "pub", "", "")
	fs.stringVar(&cfg.Sub, "sub", "", "")

	// DTLS params
	fs.stringVar(&cfg.DTLS, "dtls", "", "")
	fs.stringVar(&cfg.DTLSState, "dtls-state", "", "")

	// HTTP params
	cfg.Header = make(http.Header)
	headerVal := &headerValue{header: cfg.Header}
	fs.customVar(headerVal, "header", "h")
	fs.stringVar(&cfg.Method, "method", "", "GET")

	// Other
	fs.durationVar(&cfg.Delay, "delay", "d", 0)
	cfg.Inputs = make([]int, 0)
	intsVal := &intsValue{ints: &cfg.Inputs}
	fs.customVar(intsVal, "input", "i")
	fs.boolVar(&cfg.AutoClose, "auto-close", "c", false)

	// Parse
	if err := fs.FlagSet.Parse(args); err != nil {
		return nil, err
	}

	return cfg, nil
}

type flagSet struct {
	*flag.FlagSet
}

func (f *flagSet) stringVar(p *string, name, short, value string) {
	f.FlagSet.StringVar(p, name, value, "")
	if short != "" {
		f.FlagSet.StringVar(p, short, value, "")
	}
}

func (f *flagSet) boolVar(p *bool, name, short string, value bool) {
	f.FlagSet.BoolVar(p, name, value, "")
	if short != "" {
		f.FlagSet.BoolVar(p, short, value, "")
	}
}

func (f *flagSet) durationVar(d *time.Duration, name, short string, value time.Duration) {
	f.FlagSet.DurationVar(d, name, value, "")
	if short != "" {
		f.FlagSet.DurationVar(d, short, value, "")
	}
}

func (f *flagSet) customVar(v flag.Value, name, short string) {
	f.FlagSet.Var(v, name, "")
	if short != "" {
		f.FlagSet.Var(v, short, "")
	}
}

// intsValue is a flags.Value implementation for slice of ints
type intsValue struct {
	ints *[]int
}

func (i *intsValue) String() string {
	return fmt.Sprintf("%v", i.ints)
}

func (i *intsValue) Set(value string) error {
	for _, s := range strings.Split(value, " ") {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("couldn't parse %s as integer", value)
		}
		*i.ints = append(*i.ints, n)
	}
	return nil
}

// stringsValue is a flags.Value implementation for slice of strings
type stringsValue struct {
	strings *[]string
}

func (s *stringsValue) String() string {
	return fmt.Sprintf("%v", s.strings)
}

func (s *stringsValue) Set(value string) error {
	*s.strings = append(*s.strings, value)
	return nil
}

// headerValue is a flags.Value implementation for http.Header
type headerValue struct {
	header http.Header
}

func (h *headerValue) String() string {
	return fmt.Sprintf("%v", h.header)
}

func (h *headerValue) Set(value string) error {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("could not parse header value %s", value)
	}
	k, v := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	h.header.Add(k, v)
	return nil
}
