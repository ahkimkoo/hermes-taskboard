// Probe Hermes's /v1/responses session-continuation behaviour end to end.
//
// The scenarios below exercise every failure mode that matters for taskboard:
//
//   S1  Linear chain         a → b → c with previous_response_id each step
//   S2  Skip the latest      a → b, then new request with prev=a (not b)
//                            — does Hermes see the b branch or not?
//   S3  Fork from ancestor   a → b, a → c independently (forking)
//   S4  Invalid id           prev=resp_bogus_…
//   S5  Gap in time          30-second pause between chained turns
//   S6  `conversation` field control: same input, no previous_response_id
//
// Reads the same config.yaml + .secret the running taskboard uses so creds
// come from the registered Hermes Server entry without us managing them.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/config"
)

type env struct {
	baseURL string
	apiKey  string
	model   string
}

type turn struct {
	Input              string
	PreviousResponseID string
	Conversation       string
	Stream             bool
}

type reply struct {
	Status            int
	ID                string
	Text              string
	ToolNames         []string
	RawBody           string // full JSON body, for deep-diving
	Err               error
}

func main() {
	cfgPath := flag.String("config", "/home/kasm-user/project/hermes-taskboard/data/config.yaml", "config path")
	secretPath := flag.String("secret", "/home/kasm-user/project/hermes-taskboard/data/db/.secret", "secret path")
	serverID := flag.String("server", "local", "server id")
	only := flag.String("only", "", "comma-separated scenarios to run (e.g. s1,s4); default runs all")
	flag.Parse()

	store, err := config.NewStore(*cfgPath, *secretPath)
	if err != nil {
		die("load config", err)
	}
	cfg := store.Snapshot()
	var sv *config.HermesServer
	for i := range cfg.HermesServers {
		if cfg.HermesServers[i].ID == *serverID {
			sv = &cfg.HermesServers[i]
			break
		}
	}
	if sv == nil {
		die(fmt.Sprintf("server %q not found", *serverID), nil)
	}
	e := env{
		baseURL: sv.BaseURL,
		apiKey:  sv.DecryptedAPIKey(store.Secret()),
		model:   firstNonEmptyModel(sv),
	}
	if e.apiKey == "" {
		die("no api key after decrypt", nil)
	}
	fmt.Printf("== Probe against %s  [model=%s]\n\n", e.baseURL, e.model)

	runAll := *only == ""
	want := map[string]bool{}
	for _, k := range strings.Split(*only, ",") {
		if s := strings.TrimSpace(k); s != "" {
			want[strings.ToLower(s)] = true
		}
	}
	pick := func(id string) bool { return runAll || want[strings.ToLower(id)] }

	results := []string{}
	record := func(name, verdict string) {
		results = append(results, fmt.Sprintf("  %-6s %s", verdict, name))
	}

	// -----------------------------------------------------------------------
	// S1 Linear chain — baseline that chaining works at all.
	// -----------------------------------------------------------------------
	if pick("s1") {
		header("S1  Linear chain a → b → c")
		a := send(e, turn{Input: "Remember these two facts: (1) the secret number is 42, (2) the secret colour is blue. Reply with just OK."})
		show("  Turn A", a)
		if a.Err != nil || a.ID == "" {
			record("S1", "ABORT (turn A failed)")
			goto afterS1
		}
		b := send(e, turn{Input: "And remember fact (3): the secret animal is pangolin. Reply OK.", PreviousResponseID: a.ID})
		show("  Turn B", b)
		c := send(e, turn{Input: "List all three secret facts, one per line, no extra commentary.", PreviousResponseID: b.ID})
		show("  Turn C", c)

		ok42 := containsAll(c.Text, "42")
		okBlue := containsAll(c.Text, "blue")
		okPang := containsAll(c.Text, "pangolin") || containsAll(c.Text, "Pangolin") || containsAll(c.Text, "穿山甲")
		if ok42 && okBlue && okPang {
			record("S1", "PASS")
		} else {
			record("S1", fmt.Sprintf("FAIL  42=%v blue=%v pangolin=%v", ok42, okBlue, okPang))
		}
	}
afterS1:

	// -----------------------------------------------------------------------
	// S2 Skip the latest — the "missed middle turn" question.
	// -----------------------------------------------------------------------
	if pick("s2") {
		header("S2  Skip the latest  a → b, then new turn with prev=a (not b)")
		a := send(e, turn{Input: "Remember fact (1): the secret number is 42. Reply OK."})
		show("  Turn A", a)
		if a.Err != nil || a.ID == "" {
			record("S2", "ABORT (turn A failed)")
			goto afterS2
		}
		b := send(e, turn{Input: "Remember fact (2): the secret colour is blue. Reply OK.", PreviousResponseID: a.ID})
		show("  Turn B", b)
		// Now pretend we never captured b's id (simulated "missed" middle
		// turn). Ask from the `a` anchor instead.
		c := send(e, turn{Input: "What facts have I told you so far? List everything, no speculation.", PreviousResponseID: a.ID})
		show("  Turn C (prev=a, SKIPPING b)", c)

		sees42 := containsAll(c.Text, "42")
		seesBlue := containsAll(c.Text, "blue")
		switch {
		case sees42 && !seesBlue:
			record("S2", "PASS  (expected: Hermes sees fact 1 but NOT fact 2 — proves chain is linear-by-id)")
		case sees42 && seesBlue:
			record("S2", "MIXED (sees BOTH — Hermes must also key by something other than previous_response_id, maybe model-side session memory)")
		default:
			record("S2", fmt.Sprintf("FAIL  sees42=%v seesBlue=%v", sees42, seesBlue))
		}
	}
afterS2:

	// -----------------------------------------------------------------------
	// S3 Fork — parallel children from the same parent.
	// -----------------------------------------------------------------------
	if pick("s3") {
		header("S3  Fork  a → (b and c independently)")
		a := send(e, turn{Input: "Remember fact (1): the secret number is 42. Reply OK."})
		show("  Turn A", a)
		if a.Err != nil || a.ID == "" {
			record("S3", "ABORT (turn A failed)")
			goto afterS3
		}
		b := send(e, turn{Input: "What's the secret number? Reply with just the number.", PreviousResponseID: a.ID})
		show("  Turn B (prev=a, branch 1)", b)
		c := send(e, turn{Input: "Describe the secret number in English prose, no digits.", PreviousResponseID: a.ID})
		show("  Turn C (prev=a, branch 2, independent of b)", c)

		bNum := containsAll(b.Text, "42")
		cNum := containsAll(c.Text, "forty-two") || containsAll(c.Text, "forty two") || containsAll(c.Text, "42")
		if bNum && cNum {
			record("S3", "PASS  (both branches see A; forks allowed)")
		} else {
			record("S3", fmt.Sprintf("FAIL  b_has42=%v c_describes=%v", bNum, cNum))
		}
	}
afterS3:

	// -----------------------------------------------------------------------
	// S4 Invalid id — how does Hermes handle a bogus previous_response_id?
	// -----------------------------------------------------------------------
	if pick("s4") {
		header("S4  Invalid previous_response_id")
		r := send(e, turn{
			Input:              "Reply with the single word OK.",
			PreviousResponseID: "resp_DEADBEEFcafef00d000000000000",
		})
		show("  Response", r)
		switch {
		case r.Status >= 400:
			record("S4", fmt.Sprintf("ERROR %d (Hermes rejects unknown ids)", r.Status))
		case r.ID != "":
			record("S4", "COLDSTART (Hermes silently created a new session)")
		default:
			record("S4", fmt.Sprintf("UNKNOWN status=%d id=%q", r.Status, r.ID))
		}
	}

	// -----------------------------------------------------------------------
	// S5 Gap in time — does Hermes retain state across a 30 s pause?
	// -----------------------------------------------------------------------
	if pick("s5") {
		header("S5  Gap  30-second pause between turns")
		a := send(e, turn{Input: "Remember fact: the secret number is 42. Reply OK."})
		show("  Turn A", a)
		if a.Err != nil || a.ID == "" {
			record("S5", "ABORT (turn A failed)")
			goto afterS5
		}
		fmt.Println("    (sleeping 30s to simulate a user leaving the tab…)")
		time.Sleep(30 * time.Second)
		b := send(e, turn{Input: "What secret number did I just tell you?", PreviousResponseID: a.ID})
		show("  Turn B after gap", b)
		if containsAll(b.Text, "42") {
			record("S5", "PASS  (id survives 30 s)")
		} else {
			record("S5", "FAIL  (session dropped)")
		}
	}
afterS5:

	// -----------------------------------------------------------------------
	// S7 MUTEX — sending `conversation` AND `previous_response_id` together.
	//    Real-world: taskboard used to do this on every turn after the first.
	// -----------------------------------------------------------------------
	if pick("s7") {
		header("S7  Sending both conversation AND previous_response_id together")
		a := send(e, turn{Input: "Reply OK."})
		show("  Turn A", a)
		if a.Err != nil || a.ID == "" {
			record("S7", "ABORT (turn A failed)")
			goto afterS7
		}
		r := send(e, turn{
			Input:              "Reply OK.",
			PreviousResponseID: a.ID,
			Conversation:       "tb-probe-mutex",
		})
		show("  Turn B (both fields)", r)
		if r.Status == 400 {
			record("S7", "MUTEX  (Hermes 400s — these fields are mutually exclusive, pick one)")
		} else if r.Status == 200 {
			record("S7", "COMPAT  (Hermes accepted both)")
		} else {
			record("S7", fmt.Sprintf("UNKNOWN status=%d", r.Status))
		}
	}
afterS7:

	// -----------------------------------------------------------------------
	// S6 Control — verify plain `conversation` without prev doesn't chain.
	// -----------------------------------------------------------------------
	if pick("s6") {
		header("S6  Control: `conversation` alone (no prev) — our pre-fix behaviour")
		conv := "tb-probe-" + time.Now().Format("150405")
		a := send(e, turn{Input: "Remember fact: the secret number is 42. Reply OK.", Conversation: conv})
		show("  Turn A", a)
		b := send(e, turn{Input: "What number did I just tell you?", Conversation: conv})
		show("  Turn B (same conversation tag, NO previous_response_id)", b)
		if containsAll(b.Text, "42") {
			record("S6", "UNEXPECTED PASS  (Hermes DOES respect `conversation` here)")
		} else {
			record("S6", "FAIL AS EXPECTED  (Hermes ignores `conversation`; previous_response_id is the only way)")
		}
	}

	// -----------------------------------------------------------------------
	header("Summary")
	for _, line := range results {
		fmt.Println(line)
	}
}

// -----------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------

func send(e env, t turn) reply {
	body := map[string]any{
		"model":  e.model,
		"input":  t.Input,
		"stream": false,
	}
	if t.PreviousResponseID != "" {
		body["previous_response_id"] = t.PreviousResponseID
	}
	if t.Conversation != "" {
		body["conversation"] = t.Conversation
	}
	raw, _ := json.Marshal(body)

	req, _ := http.NewRequestWithContext(context.Background(), "POST",
		strings.TrimRight(e.baseURL, "/")+"/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return reply{Err: err}
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	r := reply{Status: resp.StatusCode, RawBody: string(buf)}

	var parsed map[string]any
	if err := json.Unmarshal(buf, &parsed); err == nil {
		if s, _ := parsed["id"].(string); s != "" {
			r.ID = s
		}
		if outs, ok := parsed["output"].([]any); ok {
			for _, it := range outs {
				m, _ := it.(map[string]any)
				typ, _ := m["type"].(string)
				if typ == "message" {
					if content, ok := m["content"].([]any); ok {
						for _, c := range content {
							cm, _ := c.(map[string]any)
							t2, _ := cm["type"].(string)
							if t2 == "output_text" {
								tx, _ := cm["text"].(string)
								r.Text += tx
							}
						}
					}
				}
				if typ == "function_call" {
					n, _ := m["name"].(string)
					if n != "" {
						r.ToolNames = append(r.ToolNames, n)
					}
				}
			}
		}
	}
	return r
}

func show(label string, r reply) {
	if r.Err != nil {
		fmt.Printf("%s: ERR %v\n", label, r.Err)
		return
	}
	tools := ""
	if len(r.ToolNames) > 0 {
		tools = fmt.Sprintf("  tools=%v", r.ToolNames)
	}
	fmt.Printf("%s: status=%d id=%q%s\n", label, r.Status, r.ID, tools)
	text := strings.ReplaceAll(r.Text, "\n", " \u21b5 ")
	if len(text) > 220 {
		text = text[:220] + "…"
	}
	if text != "" {
		fmt.Printf("    text: %s\n", text)
	}
}

func header(title string) {
	fmt.Printf("\n── %s ──────────────────────────────────────────────────────\n", title)
}

func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}

func firstNonEmptyModel(sv *config.HermesServer) string {
	for _, m := range sv.Models {
		if m.IsDefault && m.Name != "" {
			return m.Name
		}
	}
	for _, m := range sv.Models {
		if m.Name != "" {
			return m.Name
		}
	}
	return "hermes-agent"
}

func die(what string, err error) {
	fmt.Fprintln(os.Stderr, "FATAL", what, err)
	os.Exit(1)
}
