// Command eval runs the golden-set evaluation comparing the staged and
// unified analysis pipelines against a real LLM endpoint. It is a local,
// on-demand tool: it never runs in CI.
//
// Usage:
//
//	EVAL_BASE_URL=https://api.example.com/v1 \
//	EVAL_API_KEY=sk-... \
//	EVAL_MODEL=some-chat-model \
//	go run ./eval [-runs 3] [-modes staged,unified] [-testdata eval/testdata]
//
// See eval/README.md for fixture format and the criteria used to decide
// which mode should be the default.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mindcache-fyi/server/internal/cache"
	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/service"
	"github.com/mindcache-fyi/server/internal/store"
)

type expectedMatch struct {
	// TopicContains identifies the extracted topic by a substring of its
	// brief or title.
	TopicContains string `json:"topic_contains"`
	// Briefs are the collection briefs the topic should be matched to.
	Briefs []string `json:"briefs"`
}

type fixtureMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// fixtureChat mirrors the on-disk chat shape. Fixtures may carry structured
// messages instead of flat content; the runner flattens them before analysis.
type fixtureChat struct {
	ChatID    string           `json:"chatId"`
	Provider  string           `json:"provider"`
	SourceURL string           `json:"sourceUrl"`
	Title     string           `json:"title"`
	Content   string           `json:"content"`
	Messages  []fixtureMessage `json:"messages"`
}

type fixture struct {
	Name       string          `json:"name"`
	Chat       fixtureChat     `json:"chat"`
	Collection []string        `json:"collection"`
	Expected   []expectedMatch `json:"expected"`
}

// countingLLM wraps an LLM and counts Generate calls.
type countingLLM struct {
	inner service.LLM
	calls int
}

func (c *countingLLM) Generate(ctx context.Context, userMessage, systemPrompt string) (string, error) {
	c.calls++
	return c.inner.Generate(ctx, userMessage, systemPrompt)
}

func (c *countingLLM) IsConfigured() bool           { return c.inner.IsConfigured() }
func (c *countingLLM) GetConfig() service.LLMConfig { return c.inner.GetConfig() }

type runResult struct {
	ok       bool
	calls    int
	elapsed  time.Duration
	hits     int
	expected int
	err      error
}

func main() {
	runs := flag.Int("runs", 3, "runs per fixture per mode")
	modes := flag.String("modes", "staged,unified", "comma-separated modes to evaluate")
	testdata := flag.String("testdata", "eval/testdata", "fixture directory")
	flag.Parse()

	baseURL := os.Getenv("EVAL_BASE_URL")
	apiKey := os.Getenv("EVAL_API_KEY")
	llmModel := os.Getenv("EVAL_MODEL")
	if baseURL == "" || llmModel == "" {
		_, _ = fmt.Fprintln(os.Stderr, "EVAL_BASE_URL and EVAL_MODEL are required (EVAL_API_KEY optional)")
		os.Exit(2)
	}

	llm, err := service.NewLLMClient(baseURL, apiKey, llmModel, 1, false, nil)
	if err != nil {
		fatalf("create LLM client: %v", err)
	}

	fixtures := loadFixtures(*testdata)
	if len(fixtures) == 0 {
		fatalf("no fixtures found in %s", *testdata)
	}

	modeList := strings.Split(*modes, ",")
	for i := range modeList {
		modeList[i] = strings.TrimSpace(modeList[i])
	}

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "FIXTURE\tMODE\tRUN\tRESULT\tCALLS\tTIME\tMATCH\n")

	sums := make(map[string]*modeSummary)

	for _, fx := range fixtures {
		for _, mode := range modeList {
			for run := 1; run <= *runs; run++ {
				res := evalOnce(fx, mode, llm)
				registerSummary(sums, fx.Name, mode, res)

				result := "ok"
				if !res.ok {
					result = "FAIL"
				}
				match := fmt.Sprintf("%d/%d", res.hits, res.expected)
				if res.expected == 0 {
					match = "-"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%s\t%s\n",
					fx.Name, mode, run, result, res.calls, res.elapsed.Round(time.Millisecond), match)
				if res.err != nil {
					_, _ = fmt.Fprintf(w, "\t\t\terr: %v\t\t\t\n", res.err)
				}
			}
		}
	}
	_ = w.Flush()

	fmt.Println("\nSummary (per fixture/mode):")
	sw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(sw, "FIXTURE\tMODE\tSUCCESS\tAVG CALLS\tAVG TIME\tMATCH RATE\n")
	keys := make([]string, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := sums[k]
		_, _ = fmt.Fprintf(sw, "%s\t%s\t%d/%d\t%.2f\t%s\t%.0f%%\n",
			s.fixture, s.mode, s.ok, s.runs,
			float64(s.calls)/float64(s.runs),
			(s.totalElapsed / time.Duration(s.runs)).Round(time.Millisecond),
			100*float64(s.hits)/float64(max(1, s.expected)))
	}
	_ = sw.Flush()
}

type modeSummary struct {
	fixture      string
	mode         string
	runs         int
	ok           int
	calls        int
	totalElapsed time.Duration
	hits         int
	expected     int
}

func registerSummary(sums map[string]*modeSummary, fixture, mode string, res runResult) {
	key := fixture + "\x00" + mode
	s, ok := sums[key]
	if !ok {
		s = &modeSummary{fixture: fixture, mode: mode}
		sums[key] = s
	}
	s.runs++
	if res.ok {
		s.ok++
	}
	s.calls += res.calls
	s.totalElapsed += res.elapsed
	s.hits += res.hits
	s.expected += res.expected
}

func evalOnce(fx fixture, mode string, llm service.LLM) runResult {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "mindcache-eval-*")
	if err != nil {
		return runResult{err: err}
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	db, err := store.OpenSQLite(filepath.Join(tmpDir, "eval.db"))
	if err != nil {
		return runResult{err: err}
	}
	defer func() { _ = db.Close() }()
	if err := store.RunMigrations(db); err != nil {
		return runResult{err: err}
	}
	repo := store.NewMindcacheRepo(db)

	// Seed the collection directly — repo.Create does not need the LLM, so
	// this does not pollute the measurement.
	for _, brief := range fx.Collection {
		if _, err := repo.Create(ctx, brief, nil); err != nil {
			return runResult{err: fmt.Errorf("seed collection: %w", err)}
		}
	}

	counter := &countingLLM{inner: llm}
	kv := cache.NewKVCache()
	defer kv.Stop()
	svc := service.NewAnalyseService(kv, repo, counter, 100000, nil, nil, 0, 0, mode)

	chat := model.Chat{
		ChatID:    fx.Chat.ChatID,
		Provider:  fx.Chat.Provider,
		SourceURL: fx.Chat.SourceURL,
		Title:     fx.Chat.Title,
		Content:   fx.Chat.Content,
	}
	if chat.Content == "" && len(fx.Chat.Messages) > 0 {
		chat.Content = flattenMessages(fx.Chat.Messages)
	}
	if chat.ChatID == "" {
		chat.ChatID = "eval-" + fx.Name
	}
	if chat.Provider == "" {
		chat.Provider = "chatgpt"
	}
	if chat.SourceURL == "" {
		chat.SourceURL = "https://example.com/eval/" + fx.Name
	}

	start := time.Now()
	res, err := svc.Analyse(ctx, chat)
	elapsed := time.Since(start)
	if err != nil {
		return runResult{calls: counter.calls, elapsed: elapsed, err: err, expected: len(fx.Expected)}
	}

	hits := 0
	for _, exp := range fx.Expected {
		topic := findTopic(res.Topics, exp.TopicContains)
		if topic == nil {
			continue
		}
		matchedBriefs := map[string]bool{}
		for _, mc := range res.Mindcaches {
			for _, id := range res.TopicMindcacheMap[topic.TopicID] {
				if mc.ID == id {
					matchedBriefs[mc.Brief] = true
				}
			}
		}
		all := true
		for _, brief := range exp.Briefs {
			if !matchedBriefs[brief] {
				all = false
				break
			}
		}
		if all {
			hits++
		}
	}

	return runResult{
		ok:       true,
		calls:    counter.calls,
		elapsed:  elapsed,
		hits:     hits,
		expected: len(fx.Expected),
	}
}

func findTopic(topics []model.Topic, contains string) *model.Topic {
	needle := strings.ToLower(contains)
	for i := range topics {
		if strings.Contains(strings.ToLower(topics[i].Brief), needle) ||
			strings.Contains(strings.ToLower(topics[i].Title), needle) {
			return &topics[i]
		}
	}
	return nil
}

func flattenMessages(messages []fixtureMessage) string {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.Role == "" || m.Role == "other" {
			parts = append(parts, m.Content)
		} else {
			parts = append(parts, m.Role+": "+m.Content)
		}
	}
	return strings.Join(parts, "\n\n-----\n\n")
}

func loadFixtures(dir string) []fixture {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fatalf("read %s: %v", dir, err)
	}
	var fixtures []fixture
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fatalf("read fixture %s: %v", e.Name(), err)
		}
		var fx fixture
		if err := json.Unmarshal(data, &fx); err != nil {
			fatalf("parse fixture %s: %v", e.Name(), err)
		}
		if fx.Name == "" {
			fx.Name = strings.TrimSuffix(e.Name(), ".json")
		}
		fixtures = append(fixtures, fx)
	}
	sort.Slice(fixtures, func(i, j int) bool { return fixtures[i].Name < fixtures[j].Name })
	return fixtures
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
