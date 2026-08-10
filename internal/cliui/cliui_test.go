package cliui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/creack/pty"
	"golang.org/x/term"
)

func TestModes(t *testing.T) {
	for _, value := range []string{"auto", "json", "plain"} {
		if _, err := ParseOutputMode(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"auto", "always", "never"} {
		if _, err := ParseColorMode(value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ParseOutputMode("yaml"); err == nil {
		t.Fatal("expected invalid output mode")
	}
	if _, err := ParseColorMode("sometimes"); err == nil {
		t.Fatal("expected invalid color mode")
	}
}

func TestProbeKeepsStreamsIndependent(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	diagnostic, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	capabilities := Probe(ProbeOptions{
		Input: input, Output: output, Diagnostic: diagnostic,
		Environment:  []string{"TERM=xterm-256color", "LANG=C", "NO_COLOR=", "CI=true", "SECONDBOX_ACCESSIBLE=1"},
		IsTerminal:   func(fd int) bool { return fd == int(diagnostic.Fd()) },
		Size:         func(int) (int, int, error) { return 132, 41, nil },
		ColorProfile: func(io.Writer, []string) colorprofile.Profile { return colorprofile.ANSI256 },
	})
	if capabilities.Output.TTY {
		t.Fatal("stdout must remain redirected")
	}
	if !capabilities.Diagnostic.TTY || capabilities.Diagnostic.Width != 132 || capabilities.Diagnostic.Color != ProfileANSI256 {
		t.Fatalf("unexpected diagnostic capabilities: %#v", capabilities.Diagnostic)
	}
	if capabilities.Unicode || !capabilities.NoColor || !capabilities.CI || !capabilities.Accessible {
		t.Fatalf("unexpected aggregate capabilities: %#v", capabilities)
	}
}

func TestCapabilityAndThemeProfiles(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	profiles := []struct {
		detected colorprofile.Profile
		want     ColorProfile
	}{{colorprofile.Ascii, ProfileASCII}, {colorprofile.ANSI, ProfileANSI}, {colorprofile.ANSI256, ProfileANSI256}, {colorprofile.TrueColor, ProfileTrueColor}}
	for _, test := range profiles {
		for _, dark := range []bool{false, true} {
			capabilities := Probe(ProbeOptions{Output: file, Environment: []string{"TERM=xterm", "LANG=en_US.UTF-8"}, IsTerminal: func(int) bool { return true }, Size: func(int) (int, int, error) { return 91, 27, nil }, ColorProfile: func(io.Writer, []string) colorprofile.Profile { return test.detected }, DarkBackground: func(*os.File, *os.File) bool { return dark }})
			if capabilities.Output.Color != test.want || capabilities.Output.Width != 91 || capabilities.Output.Height != 27 {
				t.Fatalf("profile = %#v", capabilities.Output)
			}
			wantBackground := BackgroundLight
			if dark {
				wantBackground = BackgroundDark
			}
			if capabilities.Output.Background != wantBackground || !capabilities.Unicode {
				t.Fatalf("profile = %#v", capabilities)
			}
		}
	}
	dumb := Probe(ProbeOptions{Output: file, Environment: []string{"TERM=dumb", "LANG=C"}, IsTerminal: func(int) bool { return true }})
	if !dumb.Dumb || dumb.Unicode {
		t.Fatalf("dumb profile = %#v", dumb)
	}
}

func TestColorOverridesAndAutomaticEligibility(t *testing.T) {
	capabilities := ForWriter(io.Discard, io.Discard)
	capabilities.Output.TTY = true
	capabilities.Output.Color = ProfileANSI
	capabilities.NoColor = true
	automatic := Renderer{Output: io.Discard, Diagnostic: io.Discard, Capabilities: capabilities, OutputMode: OutputAuto, ColorMode: ColorAuto}
	if automatic.StyledOutput() {
		t.Fatal("NO_COLOR must disable automatic color")
	}
	always := automatic
	always.ColorMode = ColorAlways
	if !always.StyledOutput() {
		t.Fatal("explicit always must override NO_COLOR")
	}
	never := automatic
	never.Capabilities.NoColor = false
	never.ColorMode = ColorNever
	if never.StyledOutput() {
		t.Fatal("explicit never must disable color")
	}
	ci := always
	ci.Capabilities.CI = true
	if ci.StyledOutput() {
		t.Fatal("CI must disable styled output")
	}
	plain := always
	plain.OutputMode = OutputPlain
	if plain.StyledDiagnostic() {
		t.Fatal("plain output must keep diagnostics unstyled even with --color always")
	}
}

func TestSummaryPlainHasNoControlsOrTrailingSpaces(t *testing.T) {
	var output bytes.Buffer
	renderer := Renderer{Output: &output, Diagnostic: &bytes.Buffer{}, Capabilities: ForWriter(&output, nil), OutputMode: OutputPlain, ColorMode: ColorNever}
	err := renderer.WriteSummary(Summary{Title: "Sandbox\x1b]0;bad\a", Status: StatusComplete, Pairs: []Pair{{Key: "ID", Value: "sbx_123\x1b[31m"}}, Warnings: []string{"careful"}, Next: "secondbox shell sbx_123"})
	if err != nil {
		t.Fatal(err)
	}
	value := output.String()
	for _, forbidden := range []string{"\x1b", "\x07", " \n"} {
		if strings.Contains(value, forbidden) {
			t.Fatalf("output contains %q: %q", forbidden, value)
		}
	}
}

func TestSummaryStyledProfileGoldens(t *testing.T) {
	profiles := []struct {
		name       string
		color      ColorProfile
		background Background
		want       string
	}{
		{name: "dark truecolor", color: ProfileTrueColor, background: BackgroundDark, want: "\x1b[1;38;2;124;108;242mSandbox\x1b[m\n✓ \x1b[1;38;2;46;188;120mcomplete\x1b[m\n  \x1b[38;2;166;161;179mID   \x1b[m  \x1b[38;2;244;242;255msbx_123\x1b[m\n  \x1b[38;2;166;161;179mState\x1b[m  \x1b[38;2;244;242;255mready\x1b[m\n\x1b[38;2;166;161;179mNext:\x1b[m secondbox shell sbx_123\n"},
		{name: "light truecolor", color: ProfileTrueColor, background: BackgroundLight, want: "\x1b[1;38;2;81;67;184mSandbox\x1b[m\n✓ \x1b[1;38;2;46;188;120mcomplete\x1b[m\n  \x1b[38;2;107;101;117mID   \x1b[m  \x1b[38;2;36;33;44msbx_123\x1b[m\n  \x1b[38;2;107;101;117mState\x1b[m  \x1b[38;2;36;33;44mready\x1b[m\n\x1b[38;2;107;101;117mNext:\x1b[m secondbox shell sbx_123\n"},
		{name: "dark ansi", color: ProfileANSI, background: BackgroundDark, want: "\x1b[1;35mSandbox\x1b[m\n✓ \x1b[1;32mcomplete\x1b[m\n  \x1b[90mID   \x1b[m  \x1b[37msbx_123\x1b[m\n  \x1b[90mState\x1b[m  \x1b[37mready\x1b[m\n\x1b[90mNext:\x1b[m secondbox shell sbx_123\n"},
	}
	for _, test := range profiles {
		var output bytes.Buffer
		capabilities := ForWriter(&output, io.Discard)
		capabilities.Output = StreamCapabilities{TTY: true, Width: 80, Height: 24, Color: test.color, Background: test.background}
		renderer := Renderer{Output: &output, Diagnostic: io.Discard, Capabilities: capabilities, OutputMode: OutputAuto, ColorMode: ColorAuto}
		if err := renderer.WriteSummary(Summary{Title: "Sandbox", Status: StatusComplete, Pairs: []Pair{{Key: "ID", Value: "sbx_123"}, {Key: "State", Value: "ready"}}, Next: "secondbox shell sbx_123"}); err != nil {
			t.Fatal(err)
		}
		if output.String() != test.want {
			t.Errorf("%s golden = %q", test.name, output.String())
		}
	}
}

func TestTableNarrowFallbackAndTruncation(t *testing.T) {
	table := Table{Columns: []Column{{Key: "id", Title: "ID", Priority: 0, MinWidth: 8}, {Key: "state", Title: "STATE", Priority: 1, MinWidth: 6}}, Rows: []Row{{"id": "sbx_123456789", "state": "ready"}}}
	if narrow := renderTable(table, 30, false); narrow != "ID: sbx_123456789\nSTATE: ready\n" {
		t.Fatalf("narrow:\n%s", narrow)
	}
	wide := renderTable(table, 18, true)
	if !strings.Contains(wide, "ID:") {
		t.Fatalf("expected stacked fallback: %q", wide)
	}
	if got := truncate("abcdefghij", 6, true); got != "abcde…" {
		t.Fatalf("truncate = %q", got)
	}
}

func TestJSONPassthrough(t *testing.T) {
	original := []byte("{ \"unknown\" : 1.00, \"ordered\":true }\n")
	var output bytes.Buffer
	if err := WriteJSONPassthrough(&output, original); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), original) {
		t.Fatalf("JSON changed: %q", output.Bytes())
	}
}

func TestBuildFieldRequiresExplicitTargetsAndAffirmation(t *testing.T) {
	if _, err := buildField(FieldSpec{Kind: FieldText}); err == nil {
		t.Fatal("expected missing target error")
	}
	accepted := false
	_, err := buildField(FieldSpec{Kind: FieldConfirm, BoolValue: &accepted, RequireAffirmative: true})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVisualFormPTYAcceptsPasteAndRestoresTerminal(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	value := ""
	form := HuhForm{Groups: []GroupSpec{{Title: "Review", Fields: []FieldSpec{{Kind: FieldText, Title: "Name", StringValue: &value, ValidateString: func(value string) error {
		if value == "" {
			return errors.New("required")
		}
		return nil
	}}}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- form.Run(ctx, FormHandles{Input: slave, Output: slave, Width: 70, Dark: true}) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := io.WriteString(master, "pasted-value\r"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if value != "pasted-value" {
		t.Fatalf("pasted value = %q", value)
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%#v", before) != fmt.Sprintf("%#v", after) {
		t.Fatal("Huh did not restore terminal state")
	}
}

func TestAccessibleFormReportsEOF(t *testing.T) {
	value := ""
	form := HuhForm{Groups: []GroupSpec{{Fields: []FieldSpec{{Kind: FieldText, Title: "Value", StringValue: &value, ValidateString: func(value string) error {
		if value == "" {
			return errors.New("required")
		}
		return nil
	}}}}}}
	err := form.Run(context.Background(), FormHandles{Input: strings.NewReader(""), Output: io.Discard, Accessible: true})
	if err == nil {
		t.Fatal("accessible EOF must not succeed")
	}
}

func TestAccessibleFormsPreservePastedAnswersAndStripANSI(t *testing.T) {
	input := strings.NewReader("value-one\ny\n")
	var output bytes.Buffer
	value := ""
	first := HuhForm{Groups: []GroupSpec{{Fields: []FieldSpec{{Kind: FieldText, Title: "Value", StringValue: &value}}}}}
	if err := first.Run(context.Background(), FormHandles{Input: input, Output: &output, Accessible: true}); err != nil {
		t.Fatal(err)
	}
	accepted := false
	second := HuhForm{Groups: []GroupSpec{{Fields: []FieldSpec{{Kind: FieldConfirm, Title: "Accept", BoolValue: &accepted, RequireAffirmative: true}}}}}
	if err := second.Run(context.Background(), FormHandles{Input: input, Output: &output, Accessible: true}); err != nil {
		t.Fatal(err)
	}
	if value != "value-one" || !accepted {
		t.Fatalf("pasted answers = %q, %t", value, accepted)
	}
	if strings.Contains(output.String(), "\x1b") {
		t.Fatalf("accessible output contains ANSI: %q", output.String())
	}
}

func TestPlainActivityIsBoundedAndDeterminateValidates(t *testing.T) {
	var diagnostic bytes.Buffer
	capabilities := ForWriter(io.Discard, &diagnostic)
	renderer := Renderer{Output: io.Discard, Diagnostic: &diagnostic, Capabilities: capabilities, OutputMode: OutputAuto, ColorMode: ColorAuto}
	activity, err := renderer.StartActivity(context.Background(), "Download assets")
	if err != nil {
		t.Fatal(err)
	}
	if err := activity.Complete(StatusComplete, "verified"); err != nil {
		t.Fatal(err)
	}
	if err := renderer.WriteDeterminate("Assets", 2, 4); err != nil {
		t.Fatal(err)
	}
	if err := renderer.WriteDeterminate("Assets", 5, 4); err == nil {
		t.Fatal("invalid progress must fail")
	}
	got := diagnostic.String()
	for _, text := range []string{"active", "complete", "Download assets", "verified", "Assets: 2/4"} {
		if !strings.Contains(got, text) {
			t.Fatalf("progress lacks %q: %q", text, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("plain progress contains controls: %q", got)
	}
}

func TestActivityConcurrentCancellationAndCompletion(t *testing.T) {
	for iteration := 0; iteration < 10; iteration++ {
		var diagnostic bytes.Buffer
		capabilities := ForWriter(io.Discard, &diagnostic)
		capabilities.Diagnostic = StreamCapabilities{TTY: true, Width: 80, Height: 24, Color: ProfileANSI, Background: BackgroundDark}
		renderer := Renderer{Output: io.Discard, Diagnostic: &diagnostic, Capabilities: capabilities, OutputMode: OutputAuto, ColorMode: ColorAuto}
		ctx, cancel := context.WithCancel(context.Background())
		activity, err := renderer.StartActivity(ctx, "Negotiate stream")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- activity.Complete(StatusComplete, "ready") }()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("activity completion deadlocked")
		}
	}
}

func TestStyledActivityDoesNotQueryOrMutateTerminalModes(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()

	capabilities := ForWriter(io.Discard, slave)
	capabilities.Diagnostic = StreamCapabilities{
		TTY: true, Width: 80, Height: 24, Color: ProfileANSI, Background: BackgroundDark,
	}
	renderer := Renderer{
		Output: io.Discard, Diagnostic: slave, Capabilities: capabilities,
		OutputMode: OutputAuto, ColorMode: ColorAuto,
	}
	activity, err := renderer.StartActivity(context.Background(), "Negotiate stream")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := activity.Complete(StatusComplete, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(master)
	if readErr != nil && !errors.Is(readErr, syscall.EIO) {
		t.Fatal(readErr)
	}
	assertActivityControls(t, output)
}

func assertActivityControls(t *testing.T, output []byte) {
	t.Helper()
	for index := 0; index < len(output); {
		value := output[index]
		switch {
		case value == '\r' || value == '\n' || value >= 0x20 && value != 0x7f && value != 0x9b:
			index++
		case value != 0x1b:
			t.Fatalf("styled activity emitted control byte 0x%02x: %q", value, output)
		default:
			start := index
			index++
			if index >= len(output) || output[index] != '[' {
				t.Fatalf("styled activity emitted non-CSI escape at byte %d: %q", start, output)
			}
			index++
			parameterStart := index
			for index < len(output) && (output[index] < 0x40 || output[index] > 0x7e) {
				index++
			}
			if index >= len(output) {
				t.Fatalf("styled activity emitted incomplete CSI sequence at byte %d: %q", start, output)
			}
			parameters, final := output[parameterStart:index], output[index]
			index++
			sequence := output[start:index]
			if bytes.Equal(sequence, []byte("\x1b[2K")) || bytes.Equal(sequence, []byte("\x1b[1A")) {
				continue
			}
			if final != 'm' {
				t.Fatalf("styled activity emitted non-renderer CSI sequence %q: %q", sequence, output)
			}
			for _, parameter := range parameters {
				if parameter != ';' && (parameter < '0' || parameter > '9') {
					t.Fatalf("styled activity emitted non-SGR CSI sequence %q: %q", sequence, output)
				}
			}
		}
	}
}

func TestActivityLineFitsNarrowTerminal(t *testing.T) {
	theme := NewTheme(StreamCapabilities{Color: ProfileANSI, Background: BackgroundDark}, true)
	for width := 1; width <= 40; width++ {
		for _, unicodeOK := range []bool{false, true} {
			line := activityLine(theme, "|", "Create, schedule, and execute Sandbox", width, unicodeOK)
			if !utf8.ValidString(line) {
				t.Fatalf("width %d unicode %t activity line is invalid UTF-8: %q", width, unicodeOK, line)
			}
			if got := lipgloss.Width(line); got > max(1, width-1) {
				t.Fatalf("width %d activity line occupies %d columns: %q", width, got, line)
			}
		}
	}
}

func TestTruncatePreservesUTF8AtEveryNarrowWidth(t *testing.T) {
	for width := 0; width <= 10; width++ {
		for _, unicodeOK := range []bool{false, true} {
			result := truncate("Create Sandbox", width, unicodeOK)
			if !utf8.ValidString(result) || lipgloss.Width(result) > width {
				t.Fatalf("truncate width %d unicode %t = %q", width, unicodeOK, result)
			}
		}
	}
}

func FuzzSanitizeNeverEmitsTerminalControls(f *testing.F) {
	for _, seed := range []string{"plain", "\x1b[31mred", "\x1b]8;;https://example.com\aurl\x1b]8;;\a", "line\nvalue", "世界"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		got := Sanitize(value)
		for _, forbidden := range []string{"\x1b", "\x07", "\x00", "\x9b"} {
			if strings.Contains(got, forbidden) {
				t.Fatalf("sanitize(%q) contains %q: %q", value, forbidden, got)
			}
		}
	})
}

func FuzzWidthConstrainedTableHasNoTrailingSpaces(f *testing.F) {
	f.Add("sbx_123", "ready", uint8(80))
	f.Fuzz(func(t *testing.T, id, state string, rawWidth uint8) {
		width := 20 + int(rawWidth%120)
		got := renderTable(Table{Columns: []Column{{Key: "id", Title: "ID", Priority: 0, MinWidth: 4}, {Key: "state", Title: "STATE", Priority: 1, MinWidth: 5}}, Rows: []Row{{"id": id, "state": state}}}, width, true)
		if strings.Contains(got, " \n") || strings.Contains(got, "\x1b") {
			t.Fatalf("unsafe table: %q", got)
		}
	})
}
