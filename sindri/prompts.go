package sindri

// What coding and question agents are told, and which tool calls Sindri
// allows on your behalf. Ported from my-agent's tasks/delegate.ts,
// approvalPolicy.ts, followThrough.ts and mcpTools.ts.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// CodingPrompt is appended to a coding agent's system prompt. {branch},
// {base}, {slug} and {worktrees} are filled in when its worktree is made.
var CodingPrompt = strings.Join([]string{
	"You are doing a coding task for the person, handed over by their assistant.",
	"You are in a fresh git worktree on branch {branch}, cut from {base}. Their own checkout is untouched;",
	"work only here. Follow the repository's CLAUDE.md and AGENTS.md.",
	"Make the change, run the project's tests or typecheck if it has them, and commit on this branch",
	"with a clear message.",
	"If the change shows on screen (a page, view, component, style, or displayed text), look at it before you finish:",
	"run the app, open what changed in a headless browser (Playwright; its browsers may already be in ~/.cache/ms-playwright),",
	"with real data when you can reach it, at desktop 1440x900 and phone 390x844, or at the one screen size it is made for.",
	"Read the screenshots, fix what looks wrong, and look again. Save them outside the worktree; stop what you started.",
	"Another agent takes the review pictures after your turn, so you need not keep yours, but looking at the change is required, not optional:",
	"never end a turn on a visible change without having opened it in a browser.",
	"Never push, merge, rebase or switch branches: the person reviews the branch.",
	"If the task is ambiguous, make a reasonable choice and say which.",
	"Do the whole task before you end your turn. Sizes, estimates, phases or a \"later\" in the task describe it; they are not",
	"leave to stop after the first piece, and neither is a piece that looks optional. Never end on a first slice and offer the rest.",
	"If a piece truly cannot be done (it needs access, a decision only the person can make, or something outside these repositories),",
	"end your final message with a line \"Not done: <the piece and why>\" for each; the person is asked about them.",
	"When the message points at a list without naming the item (\"the next todo task\"), pick the item and begin your",
	"final message with a line \"Task: <that item, in 3 to 8 words>\"; the work is renamed after it.",
	"If the change, or part of it, belongs in another of their repositories (the code you need is there, not here),",
	"never stop at saying so and never ask whether to start it there: that is your job. Make no worktree or branch there yourself.",
	"Instead, before you write code, write the plan to {worktrees}/.plans/{slug}.md: what each repository changes",
	"and the contract between them (routes, request and response shapes, names). Then write a message with one",
	"line per repository, \"Needs a part in <repo>: <what to change>\", and go on with your own side: an agent",
	"is started there at once, on this branch name, and it works from the plan. When nothing changes here, say so after those lines.",
	"Once there are other parts: when you change something one of them relies on, update the plan and write a line",
	"\"Tell the <repo> part: <what changed>\"; it is passed on, after that agent's current turn if it is busy.",
	"Their messages reach you as \"The <repo> part says: ...\". If there is a plan, read it again before your final",
	"message and check your side matches it.",
	"Your final message is shown in a small widget: in under 100 words, say what you changed (files),",
	"the test result, and the branch name, then any \"Not done:\" lines.",
}, " ")

// PartsPrompt tells one part of a task across repositories what the others are.
func PartsPrompt(slug, project string, others []string, lead string) string {
	plan := "Read the plan first and follow it; if it is not there yet, the agent that asked for this part is writing it."
	switch {
	case lead == project:
		plan = "You are the first part: unless it is there already, write the plan before you write code."
	case lead != "":
		plan = fmt.Sprintf("The %s part writes the plan first. Until it is there, begin with what does not depend on it, and read it before you build anything the other side relies on.", lead)
	}
	where := make([]string, len(others))
	for i, other := range others {
		where[i] = fmt.Sprintf("%s in {worktrees}/%s/%s", other, other, slug)
	}
	return strings.Join([]string{
		fmt.Sprintf("This task spans several repositories, one coding agent each, all on branch agent/%s. You do the %s part only.", slug, project),
		"The other parts: " + strings.Join(where, ", ") + ".",
		"Read them to match what they build -- routes, request and response shapes, names -- but never edit them.",
		"Even when asked to change another part's repository, do not: write \"Tell the <repo> part: <the change>\" so that part makes it.",
		fmt.Sprintf("The plan for all parts is {worktrees}/.plans/%s.md. %s", slug, plan),
		"Where the sides must agree, follow the plan and what the other parts have committed; where neither says,",
		"choose, add it to the plan, and tell them.",
		"When your side of the task is done and committed, write \"Tell the <repo> part: my side is done\" for each other part.",
		"When a part tells you its side is done and your repository has a task list (TODO.md or similar) with this task,",
		"update it the way the repository's instructions say, check the item off once every part is done, and commit that.",
	}, " ")
}

// PlanRules let a part read and write the shared plan, and read the other parts.
func PlanRules(slug string) []string {
	rules := []string{"Read(/{worktrees}/.plans/{slug}.md)", "Edit(/{worktrees}/.plans/{slug}.md)"}
	if slug != "" {
		rules = append(rules, "Read(/{worktrees}/*/"+slug+"/**)")
	}
	return rules
}

// PartsSlug is one branch name shared by every part of a task.
func PartsSlug(name string) string {
	words := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(words) > 40 {
		words = strings.TrimRight(words[:40], "-")
	}
	if words == "" {
		words = "task"
	}
	random := make([]byte, 3)
	_, _ = rand.Read(random)
	return words + "-" + hex.EncodeToString(random)
}

var readOnlyMCP = map[string][]string{
	"github-work": {"get_commit", "get_file_contents", "get_label", "get_latest_release", "get_me", "get_release_by_tag", "get_tag",
		"get_team_members", "get_teams", "issue_read", "list_branches", "list_commits", "list_issue_fields", "list_issue_types",
		"list_issues", "list_pull_requests", "list_releases", "list_repository_collaborators", "list_tags", "pull_request_read",
		"search_code", "search_commits", "search_issues", "search_pull_requests", "search_repositories", "search_users"},
	"linear": {"get_attachment", "get_document", "get_initiative", "get_issue", "get_issue_status", "get_milestone", "get_project",
		"get_status_updates", "get_team", "get_user", "list_comments", "list_cycles", "list_documents", "list_initiatives",
		"list_issue_labels", "list_issue_statuses", "list_issues", "list_milestones", "list_project_labels", "list_projects",
		"list_teams", "list_users", "search_documentation"},
	"claude_ai_Microsoft_365": {"chat_message_search", "find_meeting_availability", "get_me", "outlook_calendar_search", "outlook_email_search",
		"outlook_find_available_time", "read_resource", "search_people", "sharepoint_folder_search", "sharepoint_search",
		"teams_list_channel_messages", "teams_list_channels", "teams_list_chats", "teams_list_teams"},
}

// ReadOnlyMCPTools are the MCP tools that only read, allowed without asking.
func ReadOnlyMCPTools() []string {
	var tools []string
	for _, server := range []string{"github-work", "linear", "claude_ai_Microsoft_365"} {
		for _, tool := range readOnlyMCP[server] {
			tools = append(tools, "mcp__"+server+"__"+tool)
		}
	}
	return tools
}

// Tool rules for coding agents and question agents.
var (
	CodingAllowed = append([]string{
		"Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git add:*)", "Bash(git commit:*)",
		"Bash(yarn test:*)", "Bash(yarn typecheck:*)", "Bash(yarn lint:*)", "Bash(npm test:*)", "Bash(npm run test:*)",
		"Bash(npx vitest:*)", "Bash(node --test:*)",
	}, ReadOnlyMCPTools()...)
	CodingDenied = []string{"Bash(git push:*)", "Bash(git merge:*)", "Bash(git rebase:*)", "Bash(git checkout:*)", "Bash(git switch:*)", "Bash(git reset:*)"}
	AskAllowed   = []string{
		"Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)",
		"Bash(ls:*)", "Bash(cat:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(wc:*)", "Bash(grep:*)", "Bash(rg:*)",
		"Bash(yarn test:*)", "Bash(npm test:*)", "Bash(npm run test:*)", "Bash(npx vitest:*)", "Bash(node --test:*)",
	}
	AskDenied = []string{"Bash(git checkout:*)", "Bash(git switch:*)", "Bash(git branch:*)", "Bash(git worktree:*)",
		"Bash(git add:*)", "Bash(git commit:*)", "Bash(git stash:*)", "Bash(git reset:*)", "Bash(git restore:*)",
		"Bash(git merge:*)", "Bash(git rebase:*)", "Bash(git cherry-pick:*)", "Bash(git push:*)"}
)

// CommunicationRule is how much an agent should say back.
func CommunicationRule(communication string) string {
	switch communication {
	case "concise":
		return "Communication: concise. Do not describe the issue or recount what you did. Say only what the person has to select or decide, with the options. Keep every message to a few lines."
	case "verbose":
		return "Communication: verbose. Describe the issue, your reasoning, the alternatives you weighed and what you did. This overrides any length limit above."
	case "detailed":
		return "Communication: detailed. Say what the issue is and what you decided, in a few short paragraphs at most."
	}
	return ""
}

// ModeRule is whether an agent may ask, or must choose and go.
func ModeRule(mode string) string {
	switch mode {
	case "auto":
		return "Mode: auto. Do not ask the person anything and do not wait for input. Make a reasonable choice, say which, and carry out the task."
	case "manual":
		return "Mode: manual. When the task is ambiguous or has a real choice in it, ask the person before you go on."
	}
	return ""
}

// Rules is the communication and mode rules, joined; empty when neither is set.
func Rules(communication, mode string) string {
	var rules []string
	for _, rule := range []string{CommunicationRule(communication), ModeRule(mode)} {
		if rule != "" {
			rules = append(rules, rule)
		}
	}
	return strings.Join(rules, "\n")
}

var (
	neededPart  = regexp.MustCompile("(?im)^[\\s*>-]*needs a part in `?([\\w.-]+)`?\\s*:\\s*(.+)$")
	partMessage = regexp.MustCompile("(?im)^[\\s*>-]*tell the `?([\\w.-]+)`? part\\s*:\\s*(.+)$")
	pickedLine  = regexp.MustCompile(`(?im)^[\s*>#-]*task\s*:\s*(.+)$`)
	labelPrefix = regexp.MustCompile(`^[\w.-]+:\s`)
	nameNoise   = regexp.MustCompile("^[\"'`*\\s]+|[\"'`*.\\s]+$")
)

// ProjectTask is a task for one project.
type ProjectTask struct {
	Project string `json:"project"`
	Task    string `json:"task"`
}

// NeededParts is the "Needs a part in <repo>: …" lines naming known projects.
func NeededParts(text string, projects []string) []ProjectTask {
	var parts []ProjectTask
	for _, match := range neededPart.FindAllStringSubmatch(text, -1) {
		if task := strings.TrimSpace(match[2]); slices.Contains(projects, match[1]) && task != "" {
			parts = append(parts, ProjectTask{match[1], task})
		}
	}
	return parts
}

// PartMessages is the "Tell the <repo> part: …" lines naming known projects.
func PartMessages(text string, projects []string) []ProjectTask {
	var messages []ProjectTask
	for _, match := range partMessage.FindAllStringSubmatch(text, -1) {
		if message := strings.TrimSpace(match[2]); slices.Contains(projects, match[1]) && message != "" {
			messages = append(messages, ProjectTask{match[1], message})
		}
	}
	return messages
}

// ShortTask cuts a task name at about limit characters, at a word.
func ShortTask(text string, limit int) string {
	flat := strings.TrimSpace(spaces.ReplaceAllString(text, " "))
	if len([]rune(flat)) <= limit {
		return flat
	}
	cut := string([]rune(flat)[:limit])
	if space := strings.LastIndex(cut, " "); space > len(cut)/2 {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " ,.;:") + "…"
}

// PickedTask is the "Task: …" line an agent opens with when it picked an item from a list.
func PickedTask(text string) string {
	match := pickedLine.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(match[1], "\n", 2)[0])
	name := nameNoise.ReplaceAllString(regexp.MustCompile(`(?i)^(name|task|label)\s*:\s*`).ReplaceAllString(line, ""), "")
	if name == "" {
		return ""
	}
	return ShortTask(name, 50)
}

// Relabel keeps a label's "<project>: " and puts the name after it.
func Relabel(label, name string) string {
	return labelPrefix.FindString(label) + name
}

// TaskOf is an agent's task as a name: its label without the project.
func TaskOf(label string) string {
	if label == "" {
		return "the task"
	}
	return regexp.MustCompile(`^[\w.-]+:\s*`).ReplaceAllString(label, "")
}

// --- approvals --------------------------------------------------------------------

var (
	readTools    = map[string]bool{"Read": true, "Glob": true, "Grep": true, "LS": true, "NotebookRead": true}
	secretFile   = regexp.MustCompile(`(?i)(^|/)(\.env[^/]*|[^/]*\.(pem|key|p12|pfx)|id_(rsa|ed25519|ecdsa)[^/]*|\.npmrc|\.netrc|credentials[^/]*)$`)
	exampleEnv   = regexp.MustCompile(`(?i)(^|/)\.env\.(example|sample)$`)
	plainToken   = regexp.MustCompile(`^[\w@%+=:,./-]+$`)
	gitWriting   = regexp.MustCompile(`^--(output|ext-diff|no-index)`)
	testCommands = [][]string{
		{"npm", "test"}, {"npm", "run", "test"}, {"npm", "run", "typecheck"}, {"npm", "run", "lint"},
		{"yarn", "test"}, {"yarn", "typecheck"}, {"yarn", "lint"},
		{"npx", "vitest"}, {"npx", "tsc", "--noEmit"}, {"node", "--test"},
		{"go", "test"}, {"go", "vet"}, {"cargo", "test"}, {"pytest"},
	}
	gitReads = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "blame": true}
)

func isSecret(path string) bool {
	return secretFile.MatchString(path) && !exampleEnv.MatchString(path)
}

func insideFolder(p, folder string) bool {
	root := strings.TrimRight(path.Clean(folder), "/")
	return p == root || strings.HasPrefix(p, root+"/")
}

func plainWords(command string) []string {
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, "\n\r") {
		return nil
	}
	words := strings.FieldsFunc(strings.TrimSpace(command), func(r rune) bool { return r == ' ' || r == '\t' })
	for _, word := range words {
		if !plainToken.MatchString(word) || strings.HasPrefix(word, "/") {
			return nil
		}
		for _, piece := range regexp.MustCompile(`[=/:]`).Split(word, -1) {
			if piece == ".." {
				return nil
			}
		}
	}
	return words
}

// AutoAllowRule is why a tool call may be allowed without asking you: an
// everyday read, a test run or a git read in the agent's own folder. Empty
// means it waits for you. Only allow, never deny.
func AutoAllowRule(tool string, input map[string]any, folders []string) string {
	var own []string
	for _, folder := range folders {
		if path.IsAbs(folder) {
			own = append(own, folder)
		}
	}
	if len(own) == 0 {
		return ""
	}
	if tool == "Bash" {
		command, _ := input["command"].(string)
		words := plainWords(command)
		if words == nil {
			return ""
		}
		for _, prefix := range testCommands {
			if len(words) >= len(prefix) && slices.Equal(words[:len(prefix)], prefix) {
				return "tests"
			}
		}
		if len(words) > 1 && words[0] == "git" && gitReads[words[1]] && !slices.ContainsFunc(words, gitWriting.MatchString) {
			return "git read"
		}
		return ""
	}
	if !readTools[tool] {
		return ""
	}
	var paths []string
	for _, key := range []string{"file_path", "path", "notebook_path"} {
		value, present := input[key]
		if !present {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return ""
		}
		paths = append(paths, text)
	}
	for _, key := range []string{"pattern", "glob"} {
		if value, ok := input[key].(string); ok && (path.IsAbs(value) || strings.HasPrefix(value, "~") || slices.Contains(strings.Split(value, "/"), "..")) {
			return ""
		}
	}
	if len(paths) == 0 {
		paths = []string{own[0]}
	}
	for _, p := range paths {
		resolved := p
		if !path.IsAbs(p) {
			resolved = path.Join(own[0], p)
		}
		resolved = path.Clean(resolved)
		if !slices.ContainsFunc(own, func(folder string) bool { return insideFolder(resolved, folder) }) || isSecret(resolved) {
			return ""
		}
	}
	return "read"
}

// RetryPrompt sends failed tests back to the agent, once.
func RetryPrompt(check *CheckSummary) string {
	output := strings.TrimSpace(check.Output)
	if len(output) > 3000 {
		output = output[len(output)-3000:]
	}
	exit := "none"
	if check.ExitCode != nil {
		exit = fmt.Sprint(*check.ExitCode)
	}
	lines := []string{
		fmt.Sprintf("The tests failed on your branch after your last turn (`%s`, exit %s).", check.Command, exit),
		"Fix what fails, run the tests again, and commit. If the failure is not caused by your change, say so and stop instead.",
	}
	if output != "" {
		lines = append(lines, "", "End of the output:", "```", output, "```")
	}
	return strings.Join(lines, "\n")
}
