package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/fmind/fkf/core"
)

const (
	evalSchemaVersion = 1
	evalDirectory     = "evals"
	evalQueriesFile   = "queries.yaml"
	maxEvalK          = 100
	maxEvalBudget     = int(core.MaxNarrativeBytes / 4)
)

// EvalSuite is the strict, versioned retrieval acceptance contract stored in a base.
type EvalSuite struct {
	FKF             int         `yaml:"fkf"`
	K               int         `yaml:"k"`
	Budget          *int        `yaml:"budget"`
	Delivery        string      `yaml:"delivery"`
	RecallThreshold float64     `yaml:"recall_threshold"`
	Queries         []EvalQuery `yaml:"queries"`
}

// EvalQuery names one question and the URIs its top-k answer must include or exclude.
type EvalQuery struct {
	Name          string   `yaml:"name"`
	Question      string   `yaml:"question"`
	Window        Window   `yaml:"window"`
	K             *int     `yaml:"k"`
	Budget        *int     `yaml:"budget"`
	Delivery      string   `yaml:"delivery"`
	ExpectEmpty   bool     `yaml:"expect_empty"`
	ExpectedURIs  []string `yaml:"expected_uris"`
	ForbiddenURIs []string `yaml:"forbidden_uris"`
}

// EvalExpectedRank records the final delivered rank for one expected URI. Rank zero means the
// requested delivery omitted it; a rank greater than k is present but still fails the top-k gate.
type EvalExpectedRank struct {
	URI  string `json:"uri"`
	Rank int    `json:"rank"`
}

// EvalQueryResult is one reproducible recall-at-k measurement.
type EvalQueryResult struct {
	Name            string             `json:"name"`
	Question        string             `json:"question"`
	Window          Window             `json:"window"`
	Budget          int                `json:"budget"`
	Delivery        string             `json:"delivery"`
	K               int                `json:"k"`
	ExpectEmpty     bool               `json:"expect_empty"`
	Recall          float64            `json:"recall"`
	Expected        int                `json:"expected"`
	FoundExpected   int                `json:"found_expected"`
	ExpectedRanks   []EvalExpectedRank `json:"expected_ranks"`
	MissingExpected []string           `json:"missing_expected,omitempty"`
	ForbiddenFound  []string           `json:"forbidden_found,omitempty"`
	DeliveredURIs   []string           `json:"delivered_uris"`
	DeliveredBytes  int                `json:"delivered_bytes"`
	DeliveredTokens int                `json:"delivered_tokens"`
	Index           LexicalIndexUse    `json:"index"`
	InputDigest     string             `json:"input_digest"`
	RankingVersion  int                `json:"ranking_version"`
	RecallThreshold float64            `json:"recall_threshold"`
	Passed          bool               `json:"passed"`
}

// EvalReport is the complete result for evals/queries.yaml. Evaluation never writes the base.
type EvalReport struct {
	Path            string            `json:"path"`
	K               int               `json:"k"`
	Budget          int               `json:"budget"`
	Delivery        string            `json:"delivery"`
	RecallThreshold float64           `json:"recall_threshold"`
	EvaluationTime  time.Time         `json:"evaluation_time"`
	Queries         []EvalQueryResult `json:"queries"`
	PassedQueries   int               `json:"passed_queries"`
	Failed          int               `json:"failed_queries"`
	Passed          bool              `json:"passed"`
}

// Evaluate runs every declared query against stored evidence and reports recall at k. It does
// not run a provider command, fetch a body, or update a derived cache.
func Evaluate(ctx context.Context, base *Base) (*EvalReport, error) {
	suite, relative, err := loadEvalSuite(ctx, base)
	if err != nil {
		return nil, err
	}
	evaluationTime := base.Now()
	report := &EvalReport{
		Path: relative, K: suite.K, Budget: *suite.Budget, Delivery: suite.Delivery, RecallThreshold: suite.RecallThreshold,
		EvaluationTime: evaluationTime,
		Queries:        make([]EvalQueryResult, 0, len(suite.Queries)),
	}
	for _, query := range suite.Queries {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		result, err := evaluateQuery(ctx, base, suite, query, evaluationTime)
		if err != nil {
			return nil, fmt.Errorf("evaluate %s: %w", query.Name, err)
		}
		report.Queries = append(report.Queries, result)
		if result.Passed {
			report.PassedQueries++
		} else {
			report.Failed++
		}
	}
	report.Passed = report.Failed == 0
	return report, nil
}

func evaluateQuery(
	ctx context.Context,
	base *Base,
	suite *EvalSuite,
	query EvalQuery,
	evaluationTime time.Time,
) (EvalQueryResult, error) {
	budget := *suite.Budget
	if query.Budget != nil {
		budget = *query.Budget
	}
	k := suite.K
	if query.K != nil {
		k = *query.K
	}
	delivery := suite.Delivery
	if query.Delivery != "" {
		delivery = query.Delivery
	}
	pack, err := BuildContext(ctx, base, ContextRequest{
		Query: query.Question, Window: query.Window, Budget: budget, DeliveryFormat: delivery,
		evaluationTime: evaluationTime,
	})
	if err != nil {
		return EvalQueryResult{}, err
	}
	delivered := make([]string, 0, len(pack.Items))
	ranks := make(map[string]int, len(pack.Items))
	for index, item := range pack.Items {
		delivered = append(delivered, item.URI)
		ranks[item.URI] = index + 1
	}
	deliveredBytes := encodedBytes(pack)
	if delivery == ContextDeliveryText {
		deliveredBytes = len(RenderContextText(pack))
	}
	result := EvalQueryResult{
		Name: query.Name, Question: query.Question, Window: pack.Receipt.Window,
		Budget: budget, Delivery: delivery, K: k, ExpectEmpty: query.ExpectEmpty, Expected: len(query.ExpectedURIs),
		ExpectedRanks: make([]EvalExpectedRank, 0, len(query.ExpectedURIs)), DeliveredURIs: delivered,
		DeliveredBytes: deliveredBytes, DeliveredTokens: bytesToTokens(deliveredBytes),
		Index:       pack.Receipt.Index,
		InputDigest: pack.Receipt.InputDigest, RankingVersion: pack.Receipt.RankingVersion,
		RecallThreshold: suite.RecallThreshold,
	}
	for _, expected := range query.ExpectedURIs {
		rank := ranks[expected]
		result.ExpectedRanks = append(result.ExpectedRanks, EvalExpectedRank{URI: expected, Rank: rank})
		if rank > 0 && rank <= k {
			result.FoundExpected++
		} else {
			result.MissingExpected = append(result.MissingExpected, expected)
		}
	}
	for _, forbidden := range query.ForbiddenURIs {
		if slices.Contains(delivered, forbidden) {
			result.ForbiddenFound = append(result.ForbiddenFound, forbidden)
		}
	}
	if query.ExpectEmpty {
		result.Recall = 1
		if len(delivered) > 0 || pack.matchedButOmitted {
			result.Recall = 0
		}
		result.Passed = len(delivered) == 0 && !pack.matchedButOmitted
	} else {
		result.Recall = float64(result.FoundExpected) / float64(result.Expected)
		result.Passed = result.Recall >= suite.RecallThreshold && len(result.ForbiddenFound) == 0
	}
	return result, nil
}

func loadEvalSuite(ctx context.Context, base *Base) (*EvalSuite, string, error) {
	relative := filepath.ToSlash(filepath.Join(evalDirectory, evalQueriesFile))
	absolute := filepath.Join(base.Root(), filepath.FromSlash(relative))
	if err := core.ValidateWithinRoot(base.Root(), absolute); err != nil {
		return nil, relative, err
	}
	data, err := core.ReadFileLimitContext(ctx, absolute, core.MaxConfigBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, relative, fmt.Errorf("%w: %s is missing; add the base's retrieval evaluation set", core.ErrConfig, relative)
		}
		return nil, relative, err
	}
	var suite EvalSuite
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&suite); err != nil {
		return nil, relative, fmt.Errorf("%w: %s: %w", core.ErrConfig, relative, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, relative, fmt.Errorf("%w: %s holds more than one YAML document", core.ErrConfig, relative)
	} else if !errors.Is(err, io.EOF) {
		return nil, relative, fmt.Errorf("%w: %s has invalid trailing YAML: %w", core.ErrConfig, relative, err)
	}
	if err := validateEvalSuite(&suite); err != nil {
		return nil, relative, fmt.Errorf("%w: %s: %w", core.ErrConfig, relative, err)
	}
	return &suite, relative, nil
}

func validateEvalSuite(suite *EvalSuite) error {
	if err := validateEvalSuiteDefaults(suite); err != nil {
		return err
	}
	names := make(map[string]struct{}, len(suite.Queries))
	for index := range suite.Queries {
		if err := validateEvalQuery(&suite.Queries[index], index, names); err != nil {
			return err
		}
	}
	return nil
}

func validateEvalSuiteDefaults(suite *EvalSuite) error {
	switch {
	case suite.FKF != evalSchemaVersion:
		return fmt.Errorf("fkf must be %d; got %d", evalSchemaVersion, suite.FKF)
	case suite.K < 1 || suite.K > maxEvalK:
		return fmt.Errorf("k is %d; expected 1..%d", suite.K, maxEvalK)
	case suite.Budget != nil && (*suite.Budget < 1 || *suite.Budget > maxEvalBudget):
		return fmt.Errorf("budget is %d; expected 1..%d when set", *suite.Budget, maxEvalBudget)
	case suite.RecallThreshold <= 0 || suite.RecallThreshold > 1:
		return fmt.Errorf("recall_threshold is %g; expected greater than 0 and at most 1", suite.RecallThreshold)
	case len(suite.Queries) == 0:
		return errors.New("queries must contain at least one evaluation")
	}
	if suite.Budget == nil {
		budget := DefaultBudget
		suite.Budget = &budget
	}
	if suite.Delivery == "" {
		suite.Delivery = ContextDeliveryJSON
	}
	if err := validateEvalDelivery(suite.Delivery); err != nil {
		return err
	}
	return nil
}

func validateEvalDelivery(delivery string) error {
	switch delivery {
	case "", ContextDeliveryJSON, ContextDeliveryJSONL, ContextDeliveryText:
		return nil
	default:
		return fmt.Errorf("delivery %q must be json, jsonl, or text", delivery)
	}
}

func validateEvalQuery(query *EvalQuery, index int, names map[string]struct{}) error {
	query.Name = strings.TrimSpace(query.Name)
	query.Question = strings.TrimSpace(query.Question)
	if query.Name == "" || query.Question == "" {
		return fmt.Errorf("queries[%d] needs non-empty name and question", index)
	}
	if _, exists := names[query.Name]; exists {
		return fmt.Errorf("queries[%d].name %q is duplicated", index, query.Name)
	}
	names[query.Name] = struct{}{}
	if err := validateEvalDelivery(query.Delivery); err != nil {
		return fmt.Errorf("queries[%d]: %w", index, err)
	}
	if query.K != nil && (*query.K < 1 || *query.K > maxEvalK) {
		return fmt.Errorf("queries[%d].k is %d; expected 1..%d when set", index, *query.K, maxEvalK)
	}
	if query.Budget != nil && (*query.Budget < 1 || *query.Budget > maxEvalBudget) {
		return fmt.Errorf("queries[%d].budget is %d; expected 1..%d when set", index, *query.Budget, maxEvalBudget)
	}
	if query.ExpectEmpty && (len(query.ExpectedURIs) > 0 || len(query.ForbiddenURIs) > 0) {
		return fmt.Errorf("queries[%d].expect_empty cannot be combined with expected_uris or forbidden_uris", index)
	}
	if !query.ExpectEmpty && len(query.ExpectedURIs) == 0 {
		return fmt.Errorf("queries[%d].expected_uris must contain at least one URI unless expect_empty is true", index)
	}
	expected, err := canonicalEvalURIs(query.ExpectedURIs)
	if err != nil {
		return fmt.Errorf("queries[%d].expected_uris: %w", index, err)
	}
	forbidden, err := canonicalEvalURIs(query.ForbiddenURIs)
	if err != nil {
		return fmt.Errorf("queries[%d].forbidden_uris: %w", index, err)
	}
	for _, uri := range forbidden {
		if slices.Contains(expected, uri) {
			return fmt.Errorf("queries[%d] URI %q is both expected and forbidden", index, uri)
		}
	}
	query.ExpectedURIs, query.ForbiddenURIs = expected, forbidden
	return nil
}

func canonicalEvalURIs(values []string) ([]string, error) {
	canonical := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		uri, err := ParseURI(value)
		if err != nil {
			return nil, err
		}
		rendered := uri.String()
		if _, exists := seen[rendered]; exists {
			return nil, fmt.Errorf("URI %q is duplicated", rendered)
		}
		seen[rendered] = struct{}{}
		canonical = append(canonical, rendered)
	}
	return canonical, nil
}
