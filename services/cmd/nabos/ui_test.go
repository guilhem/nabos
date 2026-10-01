package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/guilhem/nabos/services/internal/config"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/network"
	"golang.org/x/net/html"
)

func uiDocument(t *testing.T, w *httptest.ResponseRecorder) *html.Node {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("page: HTTP %d: %s", w.Code, w.Body.String())
	}
	doc, err := html.Parse(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func uiElements(root *html.Node, tag string) []*html.Node {
	var nodes []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (tag == "" || n.Data == tag) {
			nodes = append(nodes, n)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return nodes
}

func uiAttribute(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func uiAncestor(n *html.Node, tag string) *html.Node {
	for n = n.Parent; n != nil; n = n.Parent {
		if n.Type == html.ElementNode && n.Data == tag {
			return n
		}
	}
	return nil
}

func checkUINavigation(t *testing.T, doc *html.Node, href string) {
	t.Helper()
	var current []*html.Node
	for _, link := range uiElements(doc, "a") {
		if uiAttribute(link, "aria-current") == "page" {
			current = append(current, link)
		}
	}
	if len(current) != 1 {
		t.Fatalf("want one current navigation link, got %d", len(current))
	}
	if got := uiAttribute(current[0], "href"); got != href || uiAncestor(current[0], "nav") == nil {
		t.Fatalf("current navigation link: href=%q, want %q inside nav", got, href)
	}
}

func TestUIPageNavigation(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	h := a.routes()
	for _, path := range []string{"/", "/settings", "/wifi", "/services", "/tags", "/sounds", "/updates"} {
		for _, query := range []string{"", "?ok=%2Fwifi&err=%2Fsettings"} {
			t.Run(path+query, func(t *testing.T) {
				doc := uiDocument(t, serviceRequest(h, http.MethodGet, path+query, nil, cookie))
				checkUINavigation(t, doc, path)
			})
		}
	}
}

func checkUIFormSections(t *testing.T, doc *html.Node) {
	t.Helper()
	mains := uiElements(doc, "main")
	if len(mains) != 1 {
		t.Fatalf("want one main, got %d", len(mains))
	}
	for _, n := range uiElements(mains[0], "") {
		switch n.Data {
		case "form", "input", "select", "textarea", "button":
		default:
			continue
		}
		form := uiAncestor(n, "form")
		if n.Data == "form" {
			form = n
		}
		if form == nil {
			t.Errorf("%s %q has no form", n.Data, uiAttribute(n, "name"))
			continue
		}
		section := uiAncestor(form, "section")
		if section == nil || uiAncestor(n, "section") != section || uiAncestor(section, "section") != nil {
			t.Errorf("%s %q in form %q must share one enclosing section", n.Data, uiAttribute(n, "name"), uiAttribute(form, "action"))
		}
	}
}

func checkUISaveForm(t *testing.T, doc *html.Node, action string, subsections bool, fields ...string) {
	t.Helper()
	var forms []*html.Node
	for _, form := range uiElements(doc, "form") {
		if uiAttribute(form, "action") == action {
			forms = append(forms, form)
		}
	}
	if len(forms) != 1 {
		t.Fatalf("want one save form for %s, got %d", action, len(forms))
	}
	form := forms[0]
	var submits int
	for _, n := range uiElements(form, "") {
		typ := uiAttribute(n, "type")
		if n.Data == "button" && (typ == "" || typ == "submit") || n.Data == "input" && (typ == "submit" || typ == "image") {
			submits++
		}
	}
	if submits != 1 {
		t.Errorf("%s: want one save action, got %d", action, submits)
	}
	for _, name := range fields {
		var matches []*html.Node
		for _, n := range uiElements(form, "") {
			if uiAttribute(n, "name") == name && (n.Data == "input" || n.Data == "select" || n.Data == "textarea") {
				matches = append(matches, n)
			}
		}
		if len(matches) != 1 || uiAncestor(matches[0], "form") != form {
			t.Errorf("%s: field %q must belong to the save form exactly once", action, name)
			continue
		}
		if subsections && uiAncestor(matches[0], "fieldset") == nil {
			t.Errorf("%s: field %q has no fieldset subsection", action, name)
		}
	}
	if subsections {
		fieldsets := uiElements(form, "fieldset")
		if len(fieldsets) < 2 {
			t.Errorf("%s: want fieldset subsections", action)
		}
		for _, fieldset := range fieldsets {
			if len(uiElements(fieldset, "legend")) != 1 {
				t.Errorf("%s: fieldset must have one legend", action)
			}
		}
	}
}

func TestUIPageForms(t *testing.T) {
	core := wifiTestBus(t)
	a := testApp(t)
	cookie := serviceSession(t, a)
	core.mu.Lock()
	core.status.Phase, core.status.AttemptID = "connecting", 1
	core.mu.Unlock()
	f := appFixture(t, a)
	f.Mu.Lock()
	f.Configured = true
	f.Catalog = []device.Release{{Tag: "v1.1.0", Ready: true}}
	f.Mu.Unlock()
	if _, err := a.store.Update(func(s *config.Settings) error {
		s.Tags["d0:02:18:01:02:03:04:05"] = config.TagAction{App: "radio", Value: "https://example.org/live.mp3"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.userSoundDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.userSoundDir(), "example.mp3"), []byte("ID3"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	for _, path := range []string{"/", "/settings", "/wifi", "/services", "/tags", "/sounds", "/updates"} {
		t.Run(path, func(t *testing.T) {
			doc := uiDocument(t, serviceRequest(h, http.MethodGet, path, nil, cookie))
			checkUIFormSections(t, doc)
			switch path {
			case "/settings":
				checkUISaveForm(t, doc, "/settings/system", false, "revision", "locale", "timezone", "volume")
				checkUISaveForm(t, doc, "/settings/application", true, "chime", "wakeup", "sleep", "wakeup_6", "sleep_6", "location", "unit", "ha_enabled", "ha_host", "ha_password")
			case "/services":
				checkUISaveForm(t, doc, "/services/settings", true, "taichi_frequency", "surprise_frequency", "eightball", "books", "radio", "webhooks", "ifttt_key", "air_token", "air_index")
			}
		})
	}
}

func TestUIWifiTemplateNavigation(t *testing.T) {
	a := testApp(t)
	cookie := serviceSession(t, a)
	for _, name := range []string{"wifi-transition", "wifi-released"} {
		t.Run(name, func(t *testing.T) {
			// POST responses select navigation from the template, not the request path.
			r := httptest.NewRequest(http.MethodPost, "/wifi/connect?ok=%2Fsettings&err=%2Fservices", nil)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			a.render(w, r, name, "Wi-Fi", map[string]any{"Attempt": uint64(1), "Setup": false})
			doc := uiDocument(t, w)
			checkUINavigation(t, doc, "/wifi")
			checkUIFormSections(t, doc)
		})
	}
}

func TestUIPublicPagesHaveNoAuthenticatedNavigation(t *testing.T) {
	core := wifiTestBus(t)
	core.mu.Lock()
	core.status = network.Status{Mode: "client", Ready: true, Generation: "network:1", Address: "192.0.2.10", Phase: "idle"}
	core.mu.Unlock()
	a := testApp(t)
	h := a.routes()
	for _, path := range []string{"/setup", "/login"} {
		t.Run(path, func(t *testing.T) {
			doc := uiDocument(t, serviceRequest(h, http.MethodGet, path, nil, nil))
			if len(uiElements(doc, "nav")) != 0 {
				t.Error("unauthenticated page exposes navigation")
			}
			for _, link := range uiElements(doc, "a") {
				if uiAttribute(link, "aria-current") != "" {
					t.Error("unauthenticated page has a current navigation link")
				}
			}
			checkUIFormSections(t, doc)
		})
	}
}
