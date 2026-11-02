package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/rules"
	"github.com/gowthamgts/mailrelay/internal/storage"
)

func transferRules(ctx context.Context, cfg *models.AppConfig, exportPath, importPath string, replace bool, stdout io.Writer) error {
	if exportPath != "" && importPath != "" {
		return fmt.Errorf("-export-rules and -import-rules are mutually exclusive")
	}
	if replace && importPath == "" {
		return fmt.Errorf("-replace requires -import-rules")
	}
	var items []rules.TransferRule
	if importPath != "" {
		f, err := os.Open(importPath)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
		if err != nil {
			return err
		}
		if len(data) > 2<<20 {
			return fmt.Errorf("rules file exceeds 2 MiB")
		}
		items, err = rules.Decode(data)
		if err != nil {
			return err
		}
	}
	store, err := storage.Open(cfg.WebUI.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if importPath != "" {
		return store.ImportRules(ctx, items, replace)
	}
	records, err := store.ListRules(ctx)
	if err != nil {
		return err
	}
	format := "yaml"
	if strings.EqualFold(filepath.Ext(exportPath), ".json") {
		format = "json"
	}
	data, err := rules.Export(records, format)
	if err != nil {
		return err
	}
	if exportPath == "-" {
		_, err = stdout.Write(data)
		return err
	}
	f, err := os.OpenFile(exportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
