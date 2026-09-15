package planner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/planir"
)

// Shipped artifacts, relative to this package.
const (
	referenceWorldPath = "../../examples/worlds/reference.yaml"
	packsDir           = "../../doctrine-packs"
	referenceIntent    = "Grid-search the unexplored area to lift the fog of war"
)

func loadWorld(t *testing.T) *planir.World {
	t.Helper()
	src, err := os.ReadFile(referenceWorldPath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := planir.ParseWorld(src)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func loadPacks(t *testing.T) *doctrine.Registry {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(packsDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var packs []*doctrine.Pack
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := doctrine.Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		packs = append(packs, p)
	}
	reg, err := doctrine.NewRegistry(packs...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestSystemPromptListsEveryName(t *testing.T) {
	w, reg := loadWorld(t), loadPacks(t)
	prompt := SystemPrompt(w, reg)
	var names []string
	names = append(names, w.AreaNames()...)
	names = append(names, w.StationNames()...)
	for _, v := range w.Fleet {
		names = append(names, string(v.Caps.ID))
	}
	for _, ref := range reg.Refs() {
		names = append(names, ref.String())
	}
	for _, name := range names {
		if !strings.Contains(prompt, name) {
			t.Errorf("the prompt does not mention %s", name)
		}
	}
}

// The model is never shown a coordinate: none of the world's positions, in
// any of the forms Go would print them, appears in the prompt.
func TestSystemPromptCarriesNoCoordinate(t *testing.T) {
	w, reg := loadWorld(t), loadPacks(t)
	prompt := SystemPrompt(w, reg)
	var positions []domain.Position
	for _, a := range w.Areas {
		positions = append(positions, a.Area.Polygon.Ring...)
	}
	for _, s := range w.Stations {
		positions = append(positions, s.Position)
	}
	for _, v := range w.Fleet {
		positions = append(positions, v.State.Position)
	}
	for _, p := range positions {
		for _, f := range []float64{p.Lat, p.Lon} {
			for _, s := range []string{strconv.FormatFloat(f, 'f', -1, 64), strconv.FormatFloat(f, 'f', 3, 64)} {
				if strings.Contains(prompt, s) {
					t.Fatalf("the prompt carries the coordinate %s", s)
				}
			}
		}
	}
}

func TestOpeningIsDeterministic(t *testing.T) {
	w, reg := loadWorld(t), loadPacks(t)
	first := Opening(referenceIntent, w, reg)
	for range 20 {
		again := Opening(referenceIntent, w, reg)
		if again.System != first.System || string(again.Schema) != string(first.Schema) {
			t.Fatal("the same world produced two different prompts")
		}
	}
	if len(first.Messages) != 1 || first.Messages[0].Role != RoleUser || first.Messages[0].Content != referenceIntent {
		t.Fatalf("opening messages: %+v", first.Messages)
	}
	if string(first.Schema) != string(planir.ModelSchema()) {
		t.Fatal("the opening does not carry the model schema")
	}
}

func TestRepairAppendsReplyAndDiagnostics(t *testing.T) {
	w, reg := loadWorld(t), loadPacks(t)
	c := Opening(referenceIntent, w, reg)
	diags := []planir.Diagnostic{{Gate: planir.GateResolution, Code: "unknown_area", Pointer: "/mission/area", Message: "m", Alternatives: []string{"fog_of_war_east"}}}
	next, err := Repair(c, `{"bad": true}`, diags)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != 1 {
		t.Fatal("Repair modified the conversation it was given")
	}
	if len(next.Messages) != 3 || next.Messages[1].Role != RoleAssistant || next.Messages[2].Role != RoleUser {
		t.Fatalf("messages: %+v", next.Messages)
	}
	if next.Messages[1].Content != `{"bad": true}` {
		t.Fatalf("the refused reply is not echoed verbatim: %q", next.Messages[1].Content)
	}
	feedback := next.Messages[2].Content
	for _, want := range []string{"resolution gate", `"code":"unknown_area"`, `"pointer":"/mission/area"`, "never the constraint"} {
		if !strings.Contains(feedback, want) {
			t.Errorf("feedback lacks %q:\n%s", want, feedback)
		}
	}
}
