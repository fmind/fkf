package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/fmind/fkf/core"
)

const (
	graphGenerationBuilding = "building"
	graphGenerationCurrent  = "current"
	maxGraphGenerationBytes = 512
)

type graphGenerationState struct {
	State      string `json:"state"`
	Generation string `json:"generation"`
}

// graphGenerationSHA256 binds the publication marker to both the logical inputs and the exact
// three artifact payloads. It stays separate from generated_at so two builds in one second are
// still distinct whenever their bytes differ.
func graphGenerationSHA256(meta EdgeListMeta) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fkf-graph-generation-v1\x00"))
	for _, value := range []string{
		meta.SHA256.Inputs.Aggregate,
		meta.SHA256.Outputs.GraphTSV,
		meta.SHA256.Outputs.GraphDstTSV,
		meta.SHA256.Outputs.GraphOffsetsTSV,
	} {
		writeDigestValue(digest, []byte(value))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeGraphGenerationState(path, state, generation string) error {
	if state != graphGenerationBuilding && state != graphGenerationCurrent {
		return fmt.Errorf("invalid graph generation state %q", state)
	}
	if !isCanonicalSHA256(generation) {
		return fmt.Errorf("invalid graph generation digest %q", generation)
	}
	return core.WriteDataToJSON(graphGenerationState{State: state, Generation: generation}, path)
}

func readCurrentGraphGeneration(ctx context.Context, base *Base) (string, error) {
	data, err := base.ReadFileContext(ctx, core.GraphGenerationFile, maxGraphGenerationBytes)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("invalid derived graph cache: %s does not exist; run `fkf build graph`",
			core.GraphGenerationFile)
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", core.GraphGenerationFile, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state graphGenerationState
	if err := decoder.Decode(&state); err != nil {
		return "", fmt.Errorf("invalid derived graph cache: decode %s: %w; run `fkf build graph`",
			core.GraphGenerationFile, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return "", fmt.Errorf("invalid derived graph cache: %s holds more than one JSON document; run `fkf build graph`",
			core.GraphGenerationFile)
	} else if !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("invalid derived graph cache: decode trailing %s data: %w; run `fkf build graph`",
			core.GraphGenerationFile, err)
	}
	if state.State != graphGenerationCurrent {
		return "", fmt.Errorf("invalid derived graph cache: graph generation is %q, not current; run `fkf build graph`",
			state.State)
	}
	if !isCanonicalSHA256(state.Generation) {
		return "", fmt.Errorf("invalid derived graph cache: %s has an invalid generation digest; run `fkf build graph`",
			core.GraphGenerationFile)
	}
	return state.Generation, nil
}
