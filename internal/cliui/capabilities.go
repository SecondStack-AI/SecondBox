package cliui

import (
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/colorprofile"
	"golang.org/x/term"
)

type ColorProfile string

const (
	ProfileASCII     ColorProfile = "ascii"
	ProfileANSI      ColorProfile = "ansi"
	ProfileANSI256   ColorProfile = "ansi256"
	ProfileTrueColor ColorProfile = "truecolor"
)

type Background string

const (
	BackgroundDark  Background = "dark"
	BackgroundLight Background = "light"
)

type StreamCapabilities struct {
	TTY        bool
	Width      int
	Height     int
	Color      ColorProfile
	Background Background
}

// Capabilities intentionally describes stdin, stdout, and stderr separately.
// A redirected stdout must never disable a progress display on a TTY stderr.
type Capabilities struct {
	Input      StreamCapabilities
	Output     StreamCapabilities
	Diagnostic StreamCapabilities
	Unicode    bool
	Dumb       bool
	CI         bool
	NoColor    bool
	Accessible bool
}

type ProbeOptions struct {
	Input          *os.File
	Output         *os.File
	Diagnostic     *os.File
	Environment    []string
	Size           func(fd int) (width, height int, err error)
	IsTerminal     func(fd int) bool
	ColorProfile   func(output io.Writer, environment []string) colorprofile.Profile
	DarkBackground func(input, output *os.File) bool
}

func Probe(options ProbeOptions) Capabilities {
	env := environmentMap(options.Environment)
	size := options.Size
	if size == nil {
		size = term.GetSize
	}
	isTerminal := options.IsTerminal
	if isTerminal == nil {
		isTerminal = term.IsTerminal
	}
	profile := options.ColorProfile
	if profile == nil {
		profile = colorprofile.Detect
	}
	background := options.DarkBackground
	if background == nil {
		background = func(_, _ *os.File) bool { return true }
	}
	input := probeStream(options.Input, options.Input, env, size, isTerminal, profile, background)
	output := probeStream(options.Output, options.Input, env, size, isTerminal, profile, background)
	diagnostic := probeStream(options.Diagnostic, options.Input, env, size, isTerminal, profile, background)
	lang := strings.ToUpper(firstNonempty(env["LC_ALL"], env["LC_CTYPE"], env["LANG"]))
	unicode := lang == "" || strings.Contains(lang, "UTF-8") || strings.Contains(lang, "UTF8")
	return Capabilities{
		Input: input, Output: output, Diagnostic: diagnostic,
		Unicode:    unicode,
		Dumb:       strings.EqualFold(env["TERM"], "dumb"),
		CI:         truthy(env["CI"]),
		NoColor:    hasEnvironment(options.Environment, "NO_COLOR"),
		Accessible: truthy(env["SECONDBOX_ACCESSIBLE"]),
	}
}

func probeStream(file, input *os.File, env map[string]string, size func(int) (int, int, error), isTerminal func(int) bool, profile func(io.Writer, []string) colorprofile.Profile, dark func(*os.File, *os.File) bool) StreamCapabilities {
	result := StreamCapabilities{Width: 80, Height: 24, Color: ProfileASCII, Background: BackgroundDark}
	if file == nil || !isTerminal(int(file.Fd())) {
		return result
	}
	result.TTY = true
	if width, height, err := size(int(file.Fd())); err == nil && width > 0 && height > 0 {
		result.Width, result.Height = width, height
	}
	switch profile(file, mapToEnvironment(env)) {
	case colorprofile.TrueColor:
		result.Color = ProfileTrueColor
	case colorprofile.ANSI256:
		result.Color = ProfileANSI256
	case colorprofile.ANSI:
		result.Color = ProfileANSI
	default:
		result.Color = ProfileASCII
	}
	if !dark(input, file) {
		result.Background = BackgroundLight
	}
	return result
}

// ForWriter provides deterministic non-TTY defaults for injected buffers.
func ForWriter(output, diagnostic io.Writer) Capabilities {
	return Capabilities{
		Output:     StreamCapabilities{Width: 80, Height: 24, Color: ProfileASCII, Background: BackgroundDark},
		Diagnostic: StreamCapabilities{Width: 80, Height: 24, Color: ProfileASCII, Background: BackgroundDark},
		Unicode:    true,
	}
}

func environmentMap(entries []string) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if found {
			result[name] = value
		}
	}
	return result
}

func mapToEnvironment(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}

func hasEnvironment(entries []string, target string) bool {
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry, "=")
		if name == target {
			return true
		}
	}
	return false
}

func truthy(value string) bool {
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
