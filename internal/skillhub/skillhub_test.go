package skillhub

import "testing"

func TestParseIndex_Native(t *testing.T) {
	body := []byte(`{"name":"AOS Skills","description":"x","skills":[
	  {"slug":"web-researcher","name":"Web Researcher","description":"research the web","source":"github:injectinglabs/aos-skills/skills/web-researcher@main","tags":["research"],"verified":true},
	  {"slug":"research-table","name":"Research Table","description":"fill a table","source":"github:injectinglabs/aos-skills/skills/research-table@main"}]}`)
	entries, err := parseIndex(body, "https://raw.githubusercontent.com/injectinglabs/aos-skills/main/index.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 2 || entries[0].Slug != "web-researcher" || !entries[0].Verified {
		t.Fatalf("unexpected native parse: %+v", entries)
	}
}

func TestParseIndex_Anthropic(t *testing.T) {
	body := []byte(`{"name":"Anthropic Skills","plugins":[
	  {"name":"Docs","description":"doc skills","skills":["./skills/pdf","./skills/docx"]}]}`)
	hubURL := "https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json"
	entries, err := parseIndex(body, hubURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	want := "github:anthropics/skills/skills/pdf@main"
	if entries[0].Slug != "pdf" || entries[0].Source != want {
		t.Fatalf("anthropic resolve wrong: slug=%q source=%q (want pdf / %s)", entries[0].Slug, entries[0].Source, want)
	}
}

func TestParseIndex_Unknown(t *testing.T) {
	if _, err := parseIndex([]byte(`{"foo":"bar"}`), "https://raw.githubusercontent.com/a/b/main/x.json"); err == nil {
		t.Fatal("expected error for unsupported schema")
	}
}

func TestSearch(t *testing.T) {
	entries := []Entry{
		{Slug: "web-researcher", Name: "Web Researcher", Description: "research the web with citations"},
		{Slug: "research-table", Name: "Research Table", Description: "fill a spreadsheet by researching rows"},
		{Slug: "pdf", Name: "PDF", Description: "work with pdf documents"},
	}
	res := Search(entries, "spreadsheet table", 10)
	if len(res) == 0 || res[0].Slug != "research-table" {
		t.Fatalf("expected research-table top, got %+v", res)
	}
	// Empty query returns all (capped).
	if all := Search(entries, "", 10); len(all) != 3 {
		t.Fatalf("empty query should return all, got %d", len(all))
	}
}

func TestGithubCoordsFromRawURL(t *testing.T) {
	o, r, ref, ok := githubCoordsFromRawURL("https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json")
	if !ok || o != "anthropics" || r != "skills" || ref != "main" {
		t.Fatalf("coords: %q/%q@%q ok=%v", o, r, ref, ok)
	}
	if _, _, _, ok := githubCoordsFromRawURL("https://example.com/x.json"); ok {
		t.Fatal("non-raw-github URL should not parse")
	}
}
