package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The texts for people are pairs, German and English (golib msg). These constructors take the German text and then the English one, as their first two arguments or after leading ones (refuseStart's kind).
var pairConstructors = map[string]int{
	"msg.T":       0,
	"msg.F":       0,
	"msg.Errorf":  0,
	"refuse":      0,
	"invalid":     0,
	"refuseStart": 1,
}

// singleLanguageCalls answer another system, not people, so their texts stay German on purpose: abortMachine answers JupyterHub on its own endpoints of the hub listener and the budget tree on /v1/tree, chatError answers LibreChat in OpenAI's error format.
var singleLanguageCalls = []string{"abortMachine", "chatError"}

// singleLanguage are German texts that are no answer of the REST API or of MCP to people, each with why it stays as it is. A text is listed whole, as the literal is written.
var singleLanguage = map[string]string{
	// Stored: written once into the database, read later in the language they were written in (as os-mgt's history notes).
	"Build fehlgeschlagen, Details im Build-Log.":                                     "stored: the failure of a project image's build (Environment.Message)",
	"Build hat das Zeitlimit überschritten.":                                          "stored: Environment.Message",
	"Build nicht mehr vorhanden, Image fehlt.":                                        "stored: Environment.Message",
	"Die Architekturen ließen sich nicht zu einem Image zusammenführen (Index-Job ":   "stored: Environment.Message",
	"Der amd64-Build hat kein Image gemeldet (Zeile dhbw-digest im Build-Log fehlt).": "stored: Environment.Message",
	": Build nicht mehr vorhanden":                                                    "stored: Environment.Message",
	": kein Image gemeldet":                                                           "stored: Environment.Message",
	"%s: %d Minuten nach amd64 noch nicht fertig, abgebrochen":                        "stored: Environment.Message",
	"Nur für ": "stored: Environment.Message",
	" gebaut, GPU-Klassen anderer Architekturen werden nicht angeboten (":                                                                                              "stored: Environment.Message",
	"Das Image ist für root gebaut, läuft hier aber als UID %d (jovyan); Dateien, die root gehören, sind nicht beschreibbar.":                                          "stored: Project.ImageWarnings",
	"Das Image ist für den Benutzer %s gebaut, läuft hier aber als UID %d (jovyan); dessen Dateien sind womöglich nicht beschreibbar.":                                 "stored: Project.ImageWarnings",
	"HOME ist im Image nicht gesetzt; erhalten bleibt beim Beenden des Workspaces nur, was unter %s liegt.":                                                            "stored: Project.ImageWarnings",
	"HOME zeigt im Image auf %s statt auf %s; Dateien außerhalb von %s gehen beim Beenden des Workspaces verloren.":                                                    "stored: Project.ImageWarnings",
	"Das Image hat einen eigenen Start-Befehl (ENTRYPOINT %s); der Start-Befehl des Workspaces wird ihm nur als Argument übergeben.":                                   "stored: Project.ImageWarnings",
	"Ob das Image jupyterhub-singleuser enthält, ist nicht feststellbar (kein Image der Docker-Stacks und keine Basis von hier); ohne es startet der Workspace nicht.": "stored: Project.ImageWarnings",
	"Das Image ist %s groß (komprimiert) und damit über der Grenze von %s; der erste Start auf einem Knoten ohne das Image dauert lange.":                              "stored: Project.ImageWarnings",
	"Das Image ist %s groß (komprimiert); der erste Start auf einem Knoten ohne das Image dauert entsprechend länger.":                                                 "stored: Project.ImageWarnings",
	"; nicht automatisch vergeben: ":     "stored: the reason of a model's classification",
	" (dort gelten nur feste Eintraege)": "stored: the reason of a model's classification",
	"nicht messbar":                      "stored: the reason of a model's classification",
	"nicht messbar: ":                    "stored: the reason of a model's classification",
	"keine Werkzeugaufrufe":              "stored: the reason of a model's classification",
	// Sent to other systems: prompts for the models, job logs, Kubernetes objects, files in the homes.
	"Schreibe eine Python-Funktion is_even(n), die True zurueckgibt, wenn n gerade ist. Nur Code.": "a prompt the classification sends to a model",
	"Schreibe eine JavaScript-Funktion sum(a, b), die die Summe zurueckgibt. Nur Code.":            "a prompt the classification sends to a model",
	"Schreibe eine SQL-Abfrage, die alle Zeilen der Tabelle kunden liefert. Nur Code.":             "a prompt the classification sends to a model",
	"Antworte mit dem Wort: bereit":                              "a prompt the classification sends to a model",
	"Zaehle von 1 bis 40, nur die Zahlen, durch Komma getrennt.": "a prompt the classification sends to a model",
	"Wie ist das Wetter in Mannheim? Nutze das Werkzeug.":        "a prompt the classification sends to a model",
	"Name des Ortes": "a tool the classification offers a model",
	"Liefert das aktuelle Wetter fuer einen Ort.":                  "a tool the classification offers a model",
	"== %s Versuch %d wird gestartet (Pod wird vorbereitet)":       "a marker in a job's log",
	"== %s wartet nach Verdrängung auf eine GPU, Versuch %d folgt": "a marker in a job's log",
	"== %s wartet auf eine GPU":                                    "a marker in a job's log",
	"[Anhang „":                                                    "LibreChat: a note in the prompt sent to the model",
	"“ wurde nicht an das Modell übergeben.]":                      "LibreChat: a note in the prompt sent to the model",
	"Datei": "LibreChat: a note in the prompt sent to the model",
	"Kein Zugang zur LiteLLM-Verwaltung für ": "the admin login's HTML page in front of LiteLLM's UI, no API answer",
	// Values, not texts: states the UI maps to its own words, and their enums in the API description.
	"wird beendet":        "a state of an inference pod (InferenceTerminating), mapped by the UI",
	"wartet auf GPU":      "a state of an inference pod (InferenceWaiting), mapped by the UI",
	"nicht registriert":   "a state of a fleet machine, mapped by the UI",
	"erreichbar":          "a state of a fleet machine, mapped by the UI",
	"gestört":             "a state of a fleet machine, mapped by the UI",
	"erfüllt":             "a state of a fleet machine, mapped by the UI",
	"ohne Rückmeldung":    "a state of a fleet machine, mapped by the UI",
	"wartet auf Freigabe": "a state of a fleet machine, mapped by the UI",
	"zurückgehalten":      "a state of a fleet machine's model, mapped by the UI",
	// Names, not sentences.
	"DHBW Cloud – KI-Dienst: LLM-Keys, Workspaces und Batch-Jobs": "the MCP server's title, metadata like the tools' descriptions",
	"ohne Projekt": "the label of the usage of no project in the metrics",
	// The machines' enrolment: answered to the install script on a Mac of the fleet (MachineRouter), and its fit check, logged.
	"apiBase fehlt oder ist keine gueltige URL (transport=direct)":                                                                              "the fleet's enrolment, answered to a machine",
	"apiKey fehlt: ohne Tunnel schuetzt nur die eigene Anmeldung des Inferenzdienstes. Ein offener Endpunkt im Campusnetz ist nicht zulaessig.": "the fleet's enrolment, answered to a machine",
	"apiBase nicht zulaessig: %s":            "the fleet's enrolment, answered to a machine",
	"contact ist keine gueltige Adresse: %s": "the fleet's enrolment, answered to a machine",
	"contact fehlt: ein Rechner, dessen Inferenzdienst wir nicht steuern, braucht eine technische Kontaktadresse (Installation mit --contact).": "the fleet's enrolment, answered to a machine",
	"eine https-URL ohne Zugangsdaten, Query und Fragment ist noetig":                                                                           "the fleet's enrolment, answered to a machine",
	"%s laesst sich nicht aufloesen":              "the fleet's enrolment, answered to a machine",
	"%s zeigt auf %s, keine oeffentliche Adresse": "the fleet's enrolment, answered to a machine",
	"%s (%.1f GB) passt nicht auf %s":             "fleet.MisfitError's own text, for the log; the API answers model_too_large with its own pair",
	// LibreChat's proxy (ChatRouter): answered to LibreChat, which shows it as it is.
	"Konto und E-Mail-Adresse passen nicht zusammen.": "LibreChat",
	"LiteLLM nicht erreichbar":                        "LibreChat; also the admin login",
	"Key konnte nicht angelegt werden":                "LibreChat",
	"Anmeldung bei LiteLLM fehlgeschlagen":            "the admin login",
}

var (
	verb = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-zA-Z]`)
	// looksGerman is a German text: an umlaut or ß, or one of the words that only German texts here have.
	looksGerman = regexp.MustCompile(`[äöüÄÖÜß„]|\b(nicht|ist|darf|muss|kein|keine|keinen|sind|lässt|wird|werden|oder|und|der|die|das|dem|den|des|ein|eine|einem|einen|für|mit|nur|bereits|schon|noch|Anmeldung|fehlt|fehlen|gefunden|erreichbar|ungültig|Ungültig|Ungültiger|Ungültige|zu|auf|im|vom|zum|zur|bei|gibt|hat|haben|kann|können|angeben|erwartet|Zeichen|Bitte|bitte|wurde|sich|Fehler|Projekt|Projekte|Datei|Ordner|lesbar|geprüft|Anfrage|Inhalt|Stunden|Speicher|Zugang)\b`)
)

func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return x.Name + "." + f.Sel.Name
		}
		return f.Sel.Name
	}
	return ""
}

// isLog is a call of the logger (Infow, Warnw, Errorw, Debugw and their f and plain forms), whose texts go to the log, not to people; fmt.Errorf, errors.New and http.Error are no log calls.
func isLog(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if x, ok := sel.X.(*ast.Ident); ok && (x.Name == "fmt" || x.Name == "errors" || x.Name == "msg" || x.Name == "http") {
		return false
	}
	for _, p := range []string{"Info", "Warn", "Error", "Debug"} {
		if strings.HasPrefix(sel.Sel.Name, p) {
			return true
		}
	}
	return false
}

// stringLit is the value of a string literal; false for anything else.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func verbs(s string) []string {
	v := verb.FindAllString(strings.ReplaceAll(s, "%%", ""), -1)
	slices.Sort(v)
	return v
}

// Every text for people the API and MCP answer with has its English twin: every pair names both languages, with the same format verbs, and no German literal reaches people outside a pair. A German text that is no answer to people (stored, sent to another system, logged, a value the UI maps) is listed in singleLanguage with the reason, or goes through one of singleLanguageCalls. Log calls, struct tags and the tests are not looked at; what a test pins is the German text.
func TestMessagesForPeopleHaveAnEnglishTwin(t *testing.T) {
	fset := token.NewFileSet()
	pairs, listed := 0, map[string]bool{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && slices.Contains([]string{"generated_docs", "node_modules", "client-npm", "clients", "tmp", "helm-chart", "docs", ".git"}, d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		skip := map[*ast.BasicLit]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Field:
				if n.Tag != nil {
					skip[n.Tag] = true
				}
			case *ast.CallExpr:
				if isLog(n) || slices.Contains(singleLanguageCalls, calleeName(n)) {
					ast.Inspect(n, func(m ast.Node) bool {
						if lit, ok := m.(*ast.BasicLit); ok {
							skip[lit] = true
						}
						return true
					})
					return false
				}
				i, ok := pairConstructors[calleeName(n)]
				if !ok || len(n.Args) < i+2 {
					return true
				}
				de, okDE := stringLit(n.Args[i])
				en, okEN := stringLit(n.Args[i+1])
				if !okDE || !okEN {
					return true // a pair passed on (de, en parameters), checked where it is written
				}
				pairs++
				skip[n.Args[i].(*ast.BasicLit)], skip[n.Args[i+1].(*ast.BasicLit)] = true, true
				pos := fset.Position(n.Pos())
				switch {
				case de == "" || en == "":
					t.Errorf("%s: %s(%q, %q): a text for people needs German and English", pos, calleeName(n), de, en)
				case de == en && looksGerman.MatchString(de):
					t.Errorf("%s: %s(%q, …): the English text is the German one", pos, calleeName(n), de)
				case !slices.Equal(verbs(de), verbs(en)):
					t.Errorf("%s: %s: the format verbs differ: %v in %q, %v in %q", pos, calleeName(n), verbs(de), de, verbs(en), en)
				case looksGerman.MatchString(en) && de != en:
					t.Errorf("%s: %s: the English text looks German: %q", pos, calleeName(n), en)
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || skip[lit] {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !looksGerman.MatchString(s) || strings.HasPrefix(lit.Value, "`") && strings.Contains(s, "\n") {
				return true
			}
			if _, ok := singleLanguage[s]; ok {
				listed[s] = true
				return true
			}
			t.Errorf("%s: %q looks like a German text for people outside a pair (msg.T, msg.F, msg.Errorf, refuse, invalid, refuseStart); give it its English twin, or list it in singleLanguage with the reason it is no answer to people", fset.Position(lit.Pos()), s)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for s := range singleLanguage {
		if !listed[s] {
			t.Errorf("singleLanguage lists %q, which no longer occurs; remove it", s)
		}
	}
	t.Logf("%d texts for people in German and English", pairs)
	if pairs < 200 {
		t.Fatalf("found only %d pairs; is the scan still looking at the right calls?", pairs)
	}
}
