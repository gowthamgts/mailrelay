package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"

	"github.com/gowthamgts/mailrelay/internal/rules"
)

func (h *Handler) handleRulesExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "yaml"
	}
	if format != "yaml" && format != "json" {
		http.Error(w, "format must be yaml or json", 400)
		return
	}
	records, err := h.store.ListRules(r.Context())
	if err != nil {
		http.Error(w, "Could not read rules", 500)
		return
	}
	data, err := rules.Export(records, format)
	if err != nil {
		http.Error(w, "Could not export rules", 500)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="rules.`+format+`"`)
	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "application/yaml")
	}
	w.Write(data)
}

type importChange struct {
	Name   string   `json:"name"`
	Action string   `json:"action"`
	Fields []string `json:"fields,omitempty"`
}

func (h *Handler) handleRulesImport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		http.Error(w, "Rules file exceeds 2 MiB or could not be read", 400)
		return
	}
	items, err := rules.Decode(data)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode != "merge" && mode != "replace" {
		http.Error(w, "mode must be merge or replace", 400)
		return
	}
	records, err := h.store.ListRules(r.Context())
	if err != nil {
		http.Error(w, "Could not read rules", 500)
		return
	}
	existing := map[string]rules.TransferRule{}
	for _, record := range records {
		existing[record.Name] = rules.Portable(record)
	}
	changes := []importChange{}
	for _, item := range items {
		change := importChange{Name: item.Name, Action: "created"}
		if old, ok := existing[item.Name]; ok {
			change.Action = "unchanged"
			if old.Enabled != item.Enabled {
				change.Fields = append(change.Fields, "enabled")
			}
			if old.Position != item.Position {
				change.Fields = append(change.Fields, "position")
			}
			a, _ := json.Marshal(old.Match)
			b, _ := json.Marshal(item.Match)
			if string(a) != string(b) {
				change.Fields = append(change.Fields, "match")
			}
			if !reflect.DeepEqual(old.Webhook, item.Webhook) {
				change.Fields = append(change.Fields, "webhook")
			}
			if len(change.Fields) > 0 {
				change.Action = "updated"
			}
		}
		delete(existing, item.Name)
		changes = append(changes, change)
	}
	if mode == "replace" {
		for _, record := range records {
			if _, ok := existing[record.Name]; ok {
				changes = append(changes, importChange{Name: record.Name, Action: "deleted"})
			}
		}
	}
	if r.URL.Query().Get("apply") == "true" {
		if err := h.store.ImportRules(r.Context(), items, mode == "replace"); err != nil {
			http.Error(w, "Could not import rules", 500)
			return
		}
		if err := h.reloadRules(r.Context()); err != nil {
			http.Error(w, "Rules saved; reload failed. Restart mailrelay to load them.", 500)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(changes)
}
