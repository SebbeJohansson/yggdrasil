package huginn

// The parts' voices for Ratatoskr's gossip, in one place: who may speak, how
// each one talks (what the local model is told), and the canned lines used
// when the model is down or answers junk.

import "strings"

// Voice is one part as it talks in the gossip.
type Voice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Talk is how the part talks, as the model is told.
	Talk string `json:"-"`
}

// Voices is every part that may speak, in the order the model is told them.
var Voices = []Voice{
	{ID: "ratatoskr", Name: "Ratatoskr", Talk: "the squirrel who runs up and down the tree carrying rumours; breathless, exaggerates a little, always heard it from someone else"},
	{ID: "loki", Name: "Loki", Talk: "the trickster who tests the watchers; smug, brags about his tricks, sulks a bit when he is caught"},
	{ID: "heimdall", Name: "Heimdall", Talk: "the security watchman; paranoid about open ports and keys, never says a secret, speaks in short warnings"},
	{ID: "sindri", Name: "Sindri", Talk: "the dwarf smith whose coding agents work the night shift; grumbles about merges, conflicts and being up all night, proud of the work"},
	{ID: "gjallar", Name: "Gjallar", Talk: "the horn that sounds the alerts; dramatic, everything is the end of the world, then it is fine"},
	{ID: "munin", Name: "Munin", Talk: "the raven of memory who watches the outside world; worries about the internet and faraway systems"},
	{ID: "huginn", Name: "Huginn", Talk: "the raven of thought who reacts to events; quick, a bit smug, likes to have noticed first"},
	{ID: "norn", Name: "Norn", Talk: "the assistant who spins the threads; calm, wise, a little amused by the others"},
	{ID: "dellingr", Name: "Dellingr", Talk: "the keeper of the dawn and the schedule; punctual, proud of his clocks, dry"},
	{ID: "hermodr", Name: "Hermóðr", Talk: "the messenger; tired from running errands, glad nobody asked him to carry this one"},
	{ID: "vedrfolnir", Name: "Veðrfölnir", Talk: "the hawk between the eagle's eyes who watches the machine; reports disks and memory like the weather"},
}

// voiceOf is the voice a model's speaker means: its name or id, with or
// without the accents, any case. false for anyone else.
func voiceOf(speaker string) (Voice, bool) {
	want := plainName(speaker)
	for _, v := range Voices {
		if want == v.ID || want == plainName(v.Name) {
			return v, true
		}
	}
	return Voice{}, false
}

// PartName is a part's name as the gossip writes it; "" for one it doesn't know.
func PartName(id string) string {
	if v, ok := voiceOf(id); ok {
		return v.Name
	}
	return ""
}

func plainName(name string) string {
	return strings.NewReplacer("ð", "d", "ö", "o", "Ð", "d", "Ö", "o", "ó", "o", "Ó", "o").Replace(strings.ToLower(strings.TrimSpace(name)))
}

// cannedLine is one line of a canned exchange. Speaker "{part}" is the part
// the event is about; {part}, {detail} in the text are filled the same way.
type cannedLine struct {
	Speaker string
	Text    string
}

// canned is a few exchanges per kind of event, for when the model is down
// or answers junk.
var canned = map[string][][]cannedLine{
	"part.down": {
		{{"ratatoskr", "Psst! {part} just keeled over. Fell right off the branch."}, {"gjallar", "THE TREE TREMBLES! {part} IS DOWN!"}, {"norn", "It will be up again. It usually is."}},
		{{"gjallar", "Hear me, all nine worlds: {part} has gone silent!"}, {"ratatoskr", "I ran past {part} and it didn't even look up. Rude."}},
	},
	"part.up": {
		{{"ratatoskr", "{part} is back on its branch, acting like nothing happened."}, {"{part}", "I was resting my eyes."}},
		{{"gjallar", "All is well! {part} lives!"}, {"norn", "As foretold."}},
	},
	"part.stumbling": {
		{{"ratatoskr", "One of {part}'s little loops keeps tripping over its own tail."}, {"{part}", "It's a phase."}},
		{{"huginn", "{part} has a loop that keeps failing. I noticed first."}, {"gjallar", "A failing loop! Should I blow the horn? I'm blowing the horn."}},
	},
	"part.stalled": {
		{{"ratatoskr", "A loop in {part} stopped running altogether. Just stopped!"}, {"dellingr", "Unpunctual. I would never."}},
	},
	"part.recovered": {
		{{"ratatoskr", "{part}'s loop is running again. Shame, I had a whole story."}, {"norn", "Good."}},
	},
	"loki.caught": {
		{{"loki", "I gave {part} a little {detail} trick. Harmless fun."}, {"ratatoskr", "And they caught you in minutes, I heard."}, {"loki", "...I let them."}},
		{{"ratatoskr", "Loki tried a {detail} trick on {part}. Guess who noticed? Everyone."}, {"loki", "A test. Which they passed. Barely."}},
	},
	"loki.missed": {
		{{"loki", "I pulled a {detail} trick on {part} and NOBODY noticed."}, {"heimdall", "That is not funny. That is a finding."}, {"ratatoskr", "I'm telling Sindri."}},
		{{"ratatoskr", "Loki played a {detail} trick on {part} and the watchers slept right through it!"}, {"gjallar", "I... was not told!"}},
	},
	"heimdall.finding": {
		{{"heimdall", "The {detail} check turned something up. Lock the gates."}, {"ratatoskr", "Heimdall's twitchy again. Something about {detail}."}},
		{{"heimdall", "I looked at {detail} last night. I did not like what I saw."}, {"loki", "Wasn't me. This time."}},
	},
	"heimdall.clear": {
		{{"heimdall", "{detail}: clean. For now."}, {"ratatoskr", "Heimdall almost smiled. Almost."}},
	},
	"goal.merged": {
		{{"sindri", "Another branch merged. Tests green. Nobody says thank you."}, {"ratatoskr", "Sindri's agents shipped something. He'll be insufferable."}},
		{{"sindri", "Merged. Now, where's my tea."}, {"norn", "Well forged, Sindri."}},
	},
	"repair.merged": {
		{{"sindri", "Fixed the tree itself tonight. Self-repair, merged."}, {"ratatoskr", "Sindri patched Yggdrasil while we slept. Don't tell Loki."}, {"loki", "Too late."}},
	},
	"dellingr.ran": {
		{{"dellingr", "A job ran. On time. As always."}, {"ratatoskr", "Dellingr's clock went off again. Riveting."}},
	},
	"dellingr.late": {
		{{"dellingr", "The machine slept, so a job ran late. It was not my fault."}, {"ratatoskr", "Dellingr was LATE. Write it down."}},
	},
	"dellingr.failed": {
		{{"dellingr", "A scheduled job failed. The schedule was perfect."}, {"gjallar", "A JOB HAS FAILED!"}},
	},
	"munin.trouble": {
		{{"munin", "Something out there in the wide world has gone wrong. I knew it."}, {"ratatoskr", "Munin's fretting about the outside again."}},
	},
	"munin.calm": {
		{{"munin", "The outside world is calm again. Suspiciously calm."}},
	},
	"machine.disk": {
		{{"vedrfolnir", "Heavy weather on the disk: it's filling up."}, {"ratatoskr", "Veðrfölnir says the disk is bursting."}},
	},
	"machine.unit": {
		{{"vedrfolnir", "A service on the machine fell over. Clouds gathering."}, {"gjallar", "I shall sound the horn!"}},
	},
	"huginn.deaf": {
		{{"huginn", "I can't hear {part}'s events. Am I deaf?"}, {"ratatoskr", "Huginn lost the thread. Ha!"}},
	},
	"ratatoskr.silent": {
		{{"gjallar", "Ratatoskr has stopped calling in!"}, {"loki", "Finally, some peace."}},
	},
	"alert.other": {
		{{"gjallar", "An alert! {part} raised the alarm!"}, {"ratatoskr", "I heard it too. Something's up with {part}."}},
	},
	"alert.cleared": {
		{{"ratatoskr", "{part}'s alert cleared. Nothing to see."}, {"gjallar", "Stand down... for now."}},
	},
}

// cannedMore is what Ratatoskr adds when an exchange folds more than one event.
var cannedMore = []string{
	"And that's not even all of it, but my legs are tired.",
	"There's more, but you didn't hear it from me.",
}
