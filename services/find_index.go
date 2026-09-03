package services

import "context"

func prepareFindLexicalIndex(ctx context.Context, base *Base, filter *FindFilter) error {
	if len(filter.Grep) == 0 {
		return nil
	}
	if filter.indexFallback != "" {
		filter.index = &LexicalIndexUse{Path: LexicalIndexPath, Reason: filter.indexFallback}
		return captureFindScanGeneration(ctx, base, filter)
	}
	plan, use, err := queryFindLexicalIndex(ctx, base, *filter)
	if err != nil {
		return err
	}
	filter.index = &use
	if use.Used {
		filter.candidateURIs = plan.candidates
		filter.indexInputs = append([]LexicalInputFile(nil), plan.inputs...)
		filter.indexDigest = plan.inputsSHA256
		return nil
	}
	return captureFindScanGeneration(ctx, base, filter)
}

func captureFindScanGeneration(ctx context.Context, base *Base, filter *FindFilter) error {
	inputs, _, digest, err := lexicalInputs(ctx, base, nil)
	if err != nil {
		return err
	}
	filter.indexInputs = inputs
	filter.indexDigest = digest
	return nil
}

func findIndexInputsCurrent(ctx context.Context, base *Base, filter FindFilter) (bool, error) {
	if filter.indexDigest == "" {
		return true, nil
	}
	return lexicalInputsMatch(ctx, base, filter.indexInputs, filter.indexDigest)
}

func findScanRetry(filter FindFilter, reason string) FindFilter {
	filter.candidateURIs = nil
	filter.index = nil
	filter.indexInputs = nil
	filter.indexDigest = ""
	filter.indexFallback = reason
	filter.generationRetries++
	return filter
}
