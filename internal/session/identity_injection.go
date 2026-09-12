// Harness identity injection.
//
// Every session agent-deck spawns already carries its identity in the
// environment (AGENTDECK_INSTANCE_ID, AGENTDECK_TITLE, AGENTDECK_TOOL,
// AGENTDECK_PROFILE, AGENTDECK_ACCOUNT, ...) and can resolve the full
// database record with `agent-deck session current --json`. Those are the
// machine-readable layer. Nothing, however, told the *model* inside the
// harness that it runs under agent-deck, what it is called, who its parent
// is, or that the CLI is available — a codex/pi/gemini session asked "what
// session are you in?" answered UNKNOWN, and a claude session only knew if
// the agent-deck plugin skill happened to be installed.
//
// This file adds the model-readable layer over the existing env/CLI source
// of truth. At every start/restart the session record is rendered into a
// short instruction block (well under 40 lines) and written to an
// agent-deck-owned file under the runtime data dir; the launch command then
// hands that file to the harness through the harness's OWN non-invasive
// mechanism:
//
//	claude  --append-system-prompt-file <file>
//	codex   -c developer_instructions="<block as a TOML basic string>"
//	        (an operator's configured developer_instructions is merged in
//	        first; model_instructions_file would REPLACE the base prompt and
//	        is deliberately not used)
//	pi      --append-system-prompt <file>
//	gemini  --include-directories <dir>   (the dir holds GEMINI.md; loaded
//	        as hierarchical project memory alongside the project's own)
//	other   AGENTDECK_IDENTITY_FILE=<file> in the environment only
//
// Nothing is ever written into the user's project directory (no AGENTS.md /
// CLAUDE.md / GEMINI.md creation or edits). The block is regenerated from
// the Instance on every spawn, so a rename, re-parent or account switch is
// reflected at the next start/restart, never stale.
//
// Opt-out: `[launch] inject_identity = false` in config.toml (global) or
// `agent-deck add|launch --no-identity` (per session, persisted). SSH and
// sandboxed (docker) sessions are skipped: the file lives on the controller
// host and would not be visible where the harness actually runs.
package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"github.com/BurntSushi/toml"
)

// IdentityFileEnv is the environment variable that carries the path of the
// identity file into every spawned session (all tools, including custom
// commands, which have no instruction mechanism of their own).
const IdentityFileEnv = "AGENTDECK_IDENTITY_FILE"

// identityFileName is the canonical identity file inside the per-session
// directory. geminiIdentityFileName is the same content under the name
// gemini discovers as context when the directory is added to its workspace.
const (
	identityFileName       = "identity.md"
	geminiIdentityFileName = "GEMINI.md"
)

// identityInjectionEnabled reports whether this spawn should carry the
// model-readable identity block. Global config, per-session opt-out, and the
// remote/sandbox exclusions all gate here so every call site agrees.
func (i *Instance) identityInjectionEnabled() bool {
	if i == nil || i.IdentityInjectionDisabled {
		return false
	}
	if i.IsSSH() || i.IsSandboxed() {
		return false
	}
	cfg, _ := LoadUserConfig()
	if cfg == nil {
		return true
	}
	return cfg.Launch.GetInjectIdentity()
}

// IdentityDir returns the agent-deck-owned root that holds one directory per
// session (<runtime>/identity/<id>/).
func IdentityDir() (string, error) {
	return runtimeDataPath("identity")
}

// identitySessionDir is the per-session directory. The id is sanitized the
// same way inbox names are, so it can never escape the identity root.
func (i *Instance) identitySessionDir() (string, error) {
	root, err := IdentityDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, sanitizeInboxName(i.ID)), nil
}

// IdentityFilePath returns where the identity file for this session lives
// (whether or not it has been written yet).
func (i *Instance) IdentityFilePath() (string, error) {
	dir, err := i.identitySessionDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, identityFileName), nil
}

// BuildIdentityPrompt renders the identity block from the session record.
// Pure: no I/O, so tests can pin the exact wording. Kept deliberately short
// (LOCKED DECISION 3: under ~40 lines) — it is a pointer to the CLI, not a
// manual; the agent-deck skill and `agent-deck --help` carry the rest.
func (i *Instance) BuildIdentityPrompt() string {
	// Every record field lands on exactly one line: control characters
	// (newlines in a title, say) are collapsed so the block's line count and
	// shape are bounded regardless of what the operator typed.
	val := func(s string) string {
		s = strings.TrimSpace(identityOneLine(s))
		if s == "" {
			return "(none)"
		}
		return s
	}
	parent := "(none: this is a root session)"
	if strings.TrimSpace(i.ParentSessionID) != "" {
		parent = val(i.ParentSessionID)
	}
	profile := sessionProfileEnvValue()
	if strings.TrimSpace(profile) == "" {
		profile = DefaultProfile
	}
	// Flags-before-arguments is the one CLI convention that trips agents up
	// most (see the skill's Critical Rules), so it is stated inline.
	var b strings.Builder
	b.WriteString("# agent-deck session context\n")
	b.WriteString("You are running inside agent-deck, a terminal session manager for AI coding agents. The `agent-deck` CLI is on PATH and is how you talk to it; the same identity is in the AGENTDECK_* environment variables.\n\n")
	b.WriteString("## This session (snapshot from the agent-deck database at launch)\n")
	fmt.Fprintf(&b, "- session id: %s\n", val(i.ID))
	fmt.Fprintf(&b, "- title: %s\n", val(i.Title))
	fmt.Fprintf(&b, "- tool: %s\n", val(i.Tool))
	fmt.Fprintf(&b, "- group: %s\n", val(i.GroupPath))
	fmt.Fprintf(&b, "- profile: %s\n", profile)
	fmt.Fprintf(&b, "- account: %s\n", val(i.Account))
	fmt.Fprintf(&b, "- parent session id: %s\n", parent)
	fmt.Fprintf(&b, "- project path: %s\n", val(i.ProjectPath))
	if strings.TrimSpace(i.WorktreeBranch) != "" {
		fmt.Fprintf(&b, "- worktree branch: %s\n", val(i.WorktreeBranch))
	}
	b.WriteString("\n## agent-deck CLI (flags go BEFORE positional arguments)\n")
	b.WriteString("- `agent-deck session current --json` — this session's full, current metadata from the database (source of truth; the snapshot above may be renamed or re-parented later)\n")
	b.WriteString("- `agent-deck session send <id-or-title> \"message\"` — message another session (`--message-file FILE` for long text)\n")
	b.WriteString("- `agent-deck session output <id-or-title>` — read another session's last response\n")
	b.WriteString("- `agent-deck launch <path> -t \"Title\" -c claude --message \"prompt\"` — spawn a child session linked to you as its parent (`-no-parent` for a peer)\n")
	b.WriteString("- `agent-deck session children --json` — live status and asserted completions of your children\n")
	fmt.Fprintf(&b, "- `agent-deck inbox drain --json %s` — completion events your children queued for you\n", val(i.ID))
	b.WriteString("- `agent-deck list --json` — every session in this profile\n")
	b.WriteString("\n## Completion sentinel\n")
	b.WriteString("When a task you were given by a parent is fully done, end your final message with exactly one line:\n")
	b.WriteString("===AGENTDECK_DONE=== status=<ok|fail> summary=<one line>\n")
	b.WriteString("agent-deck forwards it to your parent as a [DONE] event; without it the parent only sees that you are waiting.\n")
	b.WriteString("\nThis block only adds context. Instructions from your operator, from project or conductor files (CLAUDE.md, AGENTS.md, GEMINI.md) and from the task you were given take precedence over it.\n")
	fmt.Fprintf(&b, "It is at $%s and is regenerated on every start/restart.\n", IdentityFileEnv)
	return b.String()
}

// identityOneLine replaces every control character (newline, tab, escape,
// ...) with a space so a record field cannot add lines or terminal control
// sequences to the block.
func identityOneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
}

// ensureIdentityFile renders the identity block and writes it into the
// per-session directory (identity.md plus GEMINI.md). It returns the
// directory and the canonical file path. An empty ok means injection is off
// for this spawn or the write failed; callers then emit nothing, so a broken
// data dir degrades to the pre-feature launch instead of a failed spawn.
//
// Writes on every call: the builders that call it run exactly once per
// start/restart, and rewriting a few hundred bytes is cheaper than any cache
// that could serve a stale title.
func (i *Instance) ensureIdentityFile() (dir, file string, ok bool) {
	if !i.identityInjectionEnabled() {
		return "", "", false
	}
	dir, err := i.identitySessionDir()
	if err != nil {
		sessionLog.Warn("identity_dir_unavailable",
			slog.String("instance_id", i.ID),
			slog.String("error", err.Error()))
		return "", "", false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		sessionLog.Warn("identity_dir_mkdir_failed",
			slog.String("path", dir),
			slog.String("error", err.Error()))
		return "", "", false
	}
	// Atomic (tmp+rename) so a harness that reads the file mid-write
	// (restart racing a slow spawn) never sees a truncated block.
	content := []byte(i.BuildIdentityPrompt())
	for _, name := range []string{identityFileName, geminiIdentityFileName} {
		path := filepath.Join(dir, name)
		if err := atomicWriteFile(path, content, 0o644); err != nil {
			sessionLog.Warn("identity_file_write_failed",
				slog.String("path", path),
				slog.String("error", err.Error()))
			return "", "", false
		}
	}
	return dir, filepath.Join(dir, identityFileName), true
}

// identityEnvExport returns the `export AGENTDECK_IDENTITY_FILE=...` fragment
// for buildEnvSourceCommand, or "" when injection is off. This is the only
// piece every tool gets, including custom `--cmd` commands.
func (i *Instance) identityEnvExport() string {
	_, file, ok := i.ensureIdentityFile()
	if !ok {
		return ""
	}
	return "export " + IdentityFileEnv + "=" + shellescape.Quote(file)
}

// claudeIdentityFlag returns the claude flag or "". Unlike the other
// harness helpers it carries no leading space: buildClaudeExtraFlagsWithName
// appends it to a flag list that is joined later, and it does so for fresh,
// resume and fork spawns alike.
func (i *Instance) claudeIdentityFlag() string {
	_, file, ok := i.ensureIdentityFile()
	if !ok {
		return ""
	}
	return "--append-system-prompt-file " + shellescape.Quote(file)
}

// codexIdentityFlag returns the codex `-c developer_instructions=...`
// override (leading space) or "". codexIdentityOverride documents the value.
func (i *Instance) codexIdentityFlag(codexHome, projectPath string) string {
	value, ok := i.codexIdentityOverride(codexHome, projectPath)
	if !ok {
		return ""
	}
	return " -c " + shellescape.Quote(value)
}

// codexIdentityOverride builds the `developer_instructions=<toml string>`
// config override for codex. The block is inlined as a TOML basic string
// (every byte escaped losslessly; see tomlBasicString), never read through a
// shell pipeline, so titles or paths containing quotes, backticks or newlines
// cannot terminate the literal or reach the shell unquoted.
//
// `-c` REPLACES a config key rather than appending to it, so the value codex
// would otherwise have used is resolved the way codex resolves it (see
// codexEffectiveDeveloperInstructions) and placed first; the identity block
// follows it. AGENTS.md discovery is a separate channel (user_instructions,
// delivered after the developer message) and is not touched by this key.
// model_instructions_file is deliberately not used: it would replace codex's
// built-in instructions.
func (i *Instance) codexIdentityOverride(codexHome, projectPath string) (string, bool) {
	_, file, ok := i.ensureIdentityFile()
	if !ok {
		return "", false
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	merged := string(content)
	if existing := codexEffectiveDeveloperInstructions(codexHome, projectPath); existing != "" {
		merged = existing + "\n\n" + merged
	}
	return "developer_instructions=" + tomlBasicString(merged), true
}

// codexConfigLayer is the subset of a codex config.toml this package reads.
type codexConfigLayer struct {
	DeveloperInstructions string                              `toml:"developer_instructions"`
	Projects              map[string]codexProjectTrustSetting `toml:"projects"`
}

type codexProjectTrustSetting struct {
	TrustLevel string `toml:"trust_level"`
}

func readCodexConfigLayer(path string) (codexConfigLayer, bool) {
	var layer codexConfigLayer
	// CodeQL go/path-injection: `path` is joined from operator-controlled
	// config (CODEX_HOME) and the session's own project path, both cleaned by
	// callers, but the taint reaches this os.ReadFile sink. Colocate the
	// canonical traversal barrier (filepath.Clean → reject "..", the form
	// CodeQL recognizes) with the sink, mirroring statEnvFileProbe in env.go.
	// A codex config with a traversal segment is a misconfiguration; failing
	// closed here only skips the developer_instructions merge, never a spawn.
	clean := filepath.Clean(path)
	if strings.Contains(clean, "..") {
		return layer, false
	}
	data, err := os.ReadFile(clean)
	if err != nil {
		return layer, false
	}
	if err := toml.Unmarshal(data, &layer); err != nil {
		return layer, false
	}
	return layer, true
}

// codexEffectiveDeveloperInstructions returns the developer_instructions
// codex would apply for a launch in projectPath under codexHome, following
// codex's own precedence (verified with `codex debug prompt-input` on
// codex-cli 0.154: a trusted project's .codex/config.toml developer message
// replaces the user-level one): the trusted project layer (closest to the
// working directory wins, searched up to the git root), then the top-level
// key of CODEX_HOME/config.toml. "" when nothing is configured. Project
// layers count only when the user config marks the project (or an ancestor)
// trusted, exactly like codex, so an untrusted checkout cannot smuggle
// instructions in through this merge. `--profile <name>.config.toml` layers
// are not resolved: agent-deck never passes --profile on the bare-command
// path, and a custom codex command is passthrough (no override at all).
func codexEffectiveDeveloperInstructions(codexHome, projectPath string) string {
	var user codexConfigLayer
	if home := strings.TrimSpace(codexHome); home != "" {
		user, _ = readCodexConfigLayer(filepath.Join(home, "config.toml"))
	}
	if project := strings.TrimSpace(projectPath); project != "" && codexProjectTrusted(user, project) {
		dir := realPathOrSelf(project)
		for {
			// CodeQL go/path-injection: dir descends from projectPath. Clean
			// and reject a surviving ".." at each filesystem sink (the config
			// read is guarded inside readCodexConfigLayer; guard the .git
			// probe here the same way), then stop. Fail-closed only skips the
			// developer_instructions merge.
			clean := filepath.Clean(dir)
			if strings.Contains(clean, "..") {
				break
			}
			if layer, ok := readCodexConfigLayer(filepath.Join(clean, ".codex", "config.toml")); ok && layer.DeveloperInstructions != "" {
				return layer.DeveloperInstructions
			}
			if _, err := os.Stat(filepath.Join(clean, ".git")); err == nil {
				break
			}
			parent := filepath.Dir(clean)
			if parent == clean {
				break
			}
			dir = parent
		}
	}
	return user.DeveloperInstructions
}

// codexProjectTrusted mirrors codex's [projects."<path>"] trust_level =
// "trusted" lookup: the project itself or any ancestor directory may carry
// the rule.
func codexProjectTrusted(user codexConfigLayer, project string) bool {
	target := realPathOrSelf(project)
	for rule, setting := range user.Projects {
		if setting.TrustLevel != "trusted" {
			continue
		}
		if isPathWithin(realPathOrSelf(ExpandPath(rule)), target) {
			return true
		}
	}
	return false
}

// tomlBasicString renders s as a TOML basic (double-quoted, single-line)
// string: backslash, double quote and every control character are escaped,
// so any input round-trips exactly through a TOML parser.
func tomlBasicString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// identityNativeArgs returns the harness-native argv that carries the
// identity block for tool, for callers that assemble argv directly instead
// of a shell string (the #2237 cross-harness launch plan). Empty when
// injection is off or the tool has no instruction mechanism (env var only).
func (i *Instance) identityNativeArgs(tool, codexHome string) []string {
	switch canonicalSwitchHarness(tool) {
	case "claude":
		_, file, ok := i.ensureIdentityFile()
		if !ok {
			return nil
		}
		return []string{"--append-system-prompt-file", file}
	case "codex":
		if strings.TrimSpace(codexHome) == "" {
			codexHome = getCodexHomeDir()
		}
		value, ok := i.codexIdentityOverride(codexHome, i.EffectiveWorkingDir())
		if !ok {
			return nil
		}
		return []string{"-c", value}
	case "pi":
		_, file, ok := i.ensureIdentityFile()
		if !ok {
			return nil
		}
		return []string{"--append-system-prompt", file}
	}
	return nil
}

// piIdentityFlag returns pi's `--append-system-prompt <file>` (pi reads a
// path argument as file contents) or "".
func (i *Instance) piIdentityFlag() string {
	_, file, ok := i.ensureIdentityFile()
	if !ok {
		return ""
	}
	return " --append-system-prompt " + shellescape.Quote(file)
}

// geminiIdentityFlag returns gemini's `--include-directories <dir>` or "".
// The per-session directory contains GEMINI.md, which gemini loads as
// project memory next to the project's own context files.
//
// Folder trust: with gemini's folder trust enabled (its default) and no
// trust rule covering the identity root, gemini opens a "Do you trust the
// following folders" dialog at startup. That dialog swallows the initial
// message `agent-deck launch -m` types (verified live: the message is lost),
// so the flag is only emitted when geminiIdentityDirTrusted positively
// establishes that no dialog will appear. Otherwise the spawn falls back to
// the env-only layer (AGENTDECK_IDENTITY_FILE) and prints a one-line pane
// hint naming the rule to add; the second return value carries that hint.
func (i *Instance) geminiIdentityFlag() (flag string, hint string) {
	dir, _, ok := i.ensureIdentityFile()
	if !ok {
		return "", ""
	}
	root, err := IdentityDir()
	if err != nil {
		return "", ""
	}
	if !geminiIdentityDirTrusted(dir) {
		sessionLog.Info("gemini_identity_dir_untrusted",
			slog.String("instance_id", i.ID),
			slog.String("identity_root", root))
		// No double quotes here: the pane command is wrapped in bash -c "..."
		// and they would surface as literal backslashes in the pane.
		return "", fmt.Sprintf("gemini: agent-deck identity context skipped because %s is not a gemini-trusted folder; add a TRUST_FOLDER rule for it to %s, or set [launch] inject_identity = false",
			root, geminiTrustedFoldersPath())
	}
	return " --include-directories " + shellescape.Quote(dir), ""
}

// geminiUserDir mirrors gemini's own home resolution: $GEMINI_CLI_HOME
// replaces $HOME as the parent of the .gemini directory.
func geminiUserDir() string {
	if home := strings.TrimSpace(os.Getenv("GEMINI_CLI_HOME")); home != "" {
		return filepath.Join(home, ".gemini")
	}
	return GetGeminiConfigDir()
}

// geminiTrustedFoldersPath mirrors gemini's Storage.getTrustedFoldersPath.
func geminiTrustedFoldersPath() string {
	if p := strings.TrimSpace(os.Getenv("GEMINI_CLI_TRUSTED_FOLDERS_PATH")); p != "" {
		return p
	}
	return filepath.Join(geminiUserDir(), "trustedFolders.json")
}

// geminiFolderTrustEnabled reads security.folderTrust.enabled from gemini's
// user settings.json; absent or unreadable means gemini's default, true.
func geminiFolderTrustEnabled() bool {
	data, err := os.ReadFile(filepath.Join(geminiUserDir(), "settings.json"))
	if err != nil {
		return true
	}
	var settings struct {
		Security struct {
			FolderTrust struct {
				Enabled *bool `json:"enabled"`
			} `json:"folderTrust"`
		} `json:"security"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return true
	}
	if settings.Security.FolderTrust.Enabled == nil {
		return true
	}
	return *settings.Security.FolderTrust.Enabled
}

// geminiIdentityDirTrusted reports whether adding dir to a gemini workspace
// is guaranteed not to raise the include-directory trust dialog: folder
// trust is off, or the longest trustedFolders.json rule covering dir grants
// trust. It re-implements gemini's LoadedTrustedFolders.isPathTrusted
// (longest rule wins; TRUST_PARENT applies to the rule's parent; DO_NOT_TRUST
// denies; no rule means "ask"). Any read/parse failure is treated as
// "would ask" so a misjudgement can only cost the injection, never the
// initial message.
func geminiIdentityDirTrusted(dir string) bool {
	if !geminiFolderTrustEnabled() {
		return true
	}
	data, err := os.ReadFile(geminiTrustedFoldersPath())
	if err != nil {
		return false
	}
	var rules map[string]string
	if err := json.Unmarshal(data, &rules); err != nil {
		return false
	}
	target := realPathOrSelf(dir)
	longest := -1
	verdict := ""
	for rulePath, level := range rules {
		effective := rulePath
		if level == "TRUST_PARENT" {
			effective = filepath.Dir(rulePath)
		}
		effective = realPathOrSelf(ExpandPath(effective))
		if !isPathWithin(effective, target) {
			continue
		}
		if len(rulePath) > longest {
			longest = len(rulePath)
			verdict = level
		}
	}
	return verdict == "TRUST_FOLDER" || verdict == "TRUST_PARENT"
}

// realPathOrSelf resolves symlinks (macOS /tmp -> /private/tmp) the way
// gemini's getRealPath does, falling back to the cleaned input.
func realPathOrSelf(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(abs)
}

// isPathWithin reports whether target equals parent or lies beneath it.
func isPathWithin(parent, target string) bool {
	parent = filepath.Clean(parent)
	target = filepath.Clean(target)
	if parent == target {
		return true
	}
	rel, err := filepath.Rel(parent, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
