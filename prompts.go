package main

import (
	"bytes"
	"embed"
	"os"
	"regexp"
	"strings"
	"text/template"
)

//go:embed prompts/*.tmpl
var promptFS embed.FS

var tmpls = template.Must(template.ParseFS(promptFS, "prompts/*.tmpl"))

type PromptData struct {
	Worktree, Branch string
	Wave             Wave
	TotalWaves       int
	Session          string
	Round            int
	Rework           bool
	ReworkStreak     int
	Defects          string
	PriorCommits     string
	PrevHandoff      string
	Gate             string
	Rules            string
	ReadonlyPaths    []string
	Steps            string
	OperatorNote     string
	Head, WaveBase   string
	Plan, Files      string
	FileLimit        int
	Browser          *BrowserBrief
}

func render(name string, d PromptData) string {
	var b bytes.Buffer
	if err := tmpls.ExecuteTemplate(&b, name+".tmpl", d); err != nil {
		panic(err) // templates are embedded and tested; a failure here is a programming error
	}
	return b.String()
}

func readFileOr(path, def string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	return string(b)
}

var (
	reStatus  = regexp.MustCompile(`(?m)^\s*\**STATUS:\s*(DONE|CONTINUE)\b`)
	reVerdict = regexp.MustCompile(`(?m)^\s*\**VERDICT:\s*(VERIFIED|REWORK|ESCALATE)\b`)
	reDefect  = regexp.MustCompile(`(?m)^\s*[-*]?\s*\**(D\d+)\**\s*[:.)]\s*(.+)$`)
)

// lastMatch returns the final occurrence, so a quoted example earlier in
// the message can never override the real closing line.
func lastMatch(re *regexp.Regexp, s string) string {
	all := re.FindAllStringSubmatch(s, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

func parseStatus(s string) string  { return lastMatch(reStatus, s) }
func parseVerdict(s string) string { return lastMatch(reVerdict, s) }

// parseDefects returns the defect block (for the next builder) and one
// headline per defect (for the non-convergence halt).
func parseDefects(s string) (block string, headlines []string) {
	var lines []string
	for _, m := range reDefect.FindAllStringSubmatch(s, -1) {
		full := strings.TrimSpace(m[2])
		lines = append(lines, m[1]+": "+full)
		head := full
		if i := strings.Index(head, " — "); i > 0 {
			head = head[:i]
		}
		headlines = append(headlines, m[1]+": "+head)
	}
	return strings.Join(lines, "\n"), headlines
}

// parsePlannerFiles pulls <<<FILE name>>> … <<<END>>> blocks.
func parsePlannerFiles(s string) map[string]string {
	re := regexp.MustCompile(`(?s)<<<FILE\s+([^>\s]+)>>>\s*\n(.*?)\n?<<<END>>>`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out[m[1]] = strings.TrimSpace(m[2]) + "\n"
	}
	return out
}
