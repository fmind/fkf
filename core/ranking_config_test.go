package core

import (
	"strings"
	"testing"
)

func TestLoadConfigRankingWeightsAndSourceRecency(t *testing.T) {
	config := `fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human label., cardinality: optional}
  topic: {description: Search topic., cardinality: many, weight: 7}
layers: {events: true}
sources:
  activity:
    enabled: true
    run: [provider, list]
    fields: {id: .id, time: .time, title: .title, topic: ".topics[]"}
    recency: {half_life_days: 14}
`
	loaded, err := LoadConfig(writeBase(t, config, nil))
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int{FieldID: 10, FieldTitle: 5, FieldTime: 1, "topic": 7} {
		if got := loaded.Schema.Weight(name); got != want {
			t.Errorf("schema weight %s = %d, want %d", name, got, want)
		}
	}
	if got := loaded.Sources["activity"].Recency.HalfLifeDays; got != 14 {
		t.Fatalf("recency half-life = %d, want 14", got)
	}
}

func TestLoadConfigRejectsInvalidRankingPolicy(t *testing.T) {
	base := `fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  topic: {description: Search topic., cardinality: optional, weight: WEIGHT}
layers: {events: true}
sources:
  activity:
    enabled: true
    run: [provider, list]
    fields: {id: .id, time: .time, topic: .topic}
    recency: {half_life_days: HALF_LIFE}
`
	for name, replacements := range map[string]map[string]string{
		"negative field weight":  {"WEIGHT": "-1", "HALF_LIFE": "14"},
		"oversized field weight": {"WEIGHT": "101", "HALF_LIFE": "14"},
		"zero half-life":         {"WEIGHT": "1", "HALF_LIFE": "0"},
		"oversized half-life":    {"WEIGHT": "1", "HALF_LIFE": "3651"},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			for old, replacement := range replacements {
				candidate = strings.ReplaceAll(candidate, old, replacement)
			}
			if _, err := LoadConfig(writeBase(t, candidate, nil)); err == nil {
				t.Fatal("LoadConfig() succeeded, want invalid ranking policy refused")
			}
		})
	}
}
