package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// applyRetry decodes a source's declared back-pressure. `on:` is required whenever more than
// one attempt is allowed, because retry-anything is how a source failing for a real reason
// becomes a source hammering a provider quietly — the reviewer has to say which failure is
// transient, and `fkf trust` prints the answer beside the command it modifies.
func applyRetry(into *RetryPolicy, file *fileRetry, fail func(string, ...any) (*Source, error)) error {
	into.Attempts = file.Attempts
	into.On = file.On
	if file.Backoff != "" {
		backoff, err := time.ParseDuration(strings.TrimSpace(file.Backoff))
		if err != nil {
			_, wrapped := fail("retry.backoff: %v", err)
			return wrapped
		}
		if backoff < 0 || backoff > MaxRetryBackoff {
			_, wrapped := fail("retry.backoff is %s; expected a duration up to %s", backoff, MaxRetryBackoff)
			return wrapped
		}
		into.Backoff = backoff
	}
	switch {
	case into.Attempts == 0 && (into.Backoff != 0 || len(into.On) > 0):
		_, wrapped := fail("retry declares backoff or on but no attempts, so nothing would ever be retried")
		return wrapped
	case into.Attempts < 0 || into.Attempts > MaxRetryAttempts:
		_, wrapped := fail("retry.attempts is %d; expected 1..%d", into.Attempts, MaxRetryAttempts)
		return wrapped
	case into.Attempts > 1 && len(into.On) == 0:
		_, wrapped := fail("retry.attempts is %d but retry.on is empty; name the failures that may be "+
			"retried (`exit:<n>` or a stderr substring) rather than retrying every failure", into.Attempts)
		return wrapped
	}
	for _, condition := range into.On {
		if strings.TrimSpace(condition) == "" {
			_, wrapped := fail("retry.on holds an empty condition")
			return wrapped
		}
		if problem := executionTextProblem(condition); problem != "" {
			_, wrapped := fail("retry.on %q %s", condition, problem)
			return wrapped
		}
		if code, found := strings.CutPrefix(condition, "exit:"); found {
			if _, err := strconv.Atoi(strings.TrimSpace(code)); err != nil {
				_, wrapped := fail("retry.on %q: %v", condition, err)
				return wrapped
			}
		}
	}
	return nil
}

func buildSource(name string, file fileSource, path string) (*Source, error) {
	fail := func(format string, args ...any) (*Source, error) {
		return nil, fmt.Errorf("%w: %s: sources.%s: %s", ErrConfig, path, name, fmt.Sprintf(format, args...))
	}
	if err := validateSourceFile(name, file, fail); err != nil {
		return nil, err
	}
	source := &Source{
		Name: name, Enabled: file.Enabled, Layer: LayerEvents, Format: FormatJSON,
		Run:  append([]string(nil), (*file.Run)...),
		Body: file.Body, Requires: append([]string(nil), file.Requires...),
		Install: strings.TrimSpace(file.Install), Window: file.Window, Fields: file.Fields,
	}
	if err := applySourceOptions(source, file, fail); err != nil {
		return nil, err
	}
	if err := applySourcePaths(source, file, fail); err != nil {
		return nil, err
	}
	return source, nil
}

type sourceBuildFail func(string, ...any) (*Source, error)

func sourceBuildError(fail sourceBuildFail, format string, args ...any) error {
	_, err := fail(format, args...)
	return err
}

func validateSourceFile(name string, file fileSource, fail sourceBuildFail) error {
	if err := ValidateSourceName(name); err != nil {
		_, wrapped := fail("%v", err)
		return wrapped
	}
	if file.Run == nil || len(*file.Run) == 0 {
		_, wrapped := fail("run is required and must contain an executable")
		return wrapped
	}
	if file.Auth != nil && len(*file.Auth) == 0 {
		_, wrapped := fail("auth must contain an executable when declared")
		return wrapped
	}
	if file.Test != nil && len(*file.Test) == 0 {
		_, wrapped := fail("test must contain an executable when declared")
		return wrapped
	}
	return nil
}

func applySourceOptions(source *Source, file fileSource, fail sourceBuildFail) error {
	if bodies := strings.TrimSpace(file.Bodies); bodies != "" {
		source.Bodies = BodyPolicy(bodies)
	}
	if file.Recency != nil {
		if file.Recency.HalfLifeDays < 1 || file.Recency.HalfLifeDays > MaxRecencyHalfLifeDays {
			return sourceBuildError(fail, "recency.half_life_days is %d; expected 1..%d",
				file.Recency.HalfLifeDays, MaxRecencyHalfLifeDays)
		}
		source.Recency.HalfLifeDays = file.Recency.HalfLifeDays
	}
	if file.Auth != nil {
		source.Auth = append([]string(nil), (*file.Auth)...)
	}
	if file.Test != nil {
		source.Test = append([]string(nil), (*file.Test)...)
	}
	if layer := strings.TrimSpace(file.Layer); layer != "" {
		if Layer(layer) != LayerEvents && Layer(layer) != LayerIndex && Layer(layer) != LayerTasks {
			return sourceBuildError(fail, "layer is %q; expected %s, %s, or %s", layer, LayerEvents, LayerIndex, LayerTasks)
		}
		source.Layer = Layer(layer)
	}
	if source.Layer == LayerTasks {
		switch {
		case strings.TrimSpace(file.Records) != "":
			return sourceBuildError(fail, "records is not valid for a tasks source; its command emits task-trace objects")
		case len(file.Fields) > 0:
			return sourceBuildError(fail, "fields is not valid for a tasks source; task traces have a closed write contract")
		case len(file.Body) > 0:
			return sourceBuildError(fail, "body is not valid for a tasks source")
		case strings.TrimSpace(file.Bodies) != "":
			return sourceBuildError(fail, "bodies is not valid for a tasks source")
		case file.Recency != nil:
			return sourceBuildError(fail, "recency is not valid for a tasks source")
		}
	}
	if format := strings.TrimSpace(file.Format); format != "" {
		if OutputFormat(format) != FormatJSON && OutputFormat(format) != FormatNDJSON {
			return sourceBuildError(fail, "format is %q; expected json or ndjson", format)
		}
		source.Format = OutputFormat(format)
	}
	if file.Timeout != "" {
		timeout, err := time.ParseDuration(strings.TrimSpace(file.Timeout))
		if err != nil {
			return sourceBuildError(fail, "timeout: %v", err)
		}
		source.Timeout = timeout
	}
	if file.MinInterval != "" {
		interval, err := time.ParseDuration(strings.TrimSpace(file.MinInterval))
		if err != nil {
			return sourceBuildError(fail, "min_interval: %v", err)
		}
		if interval <= 0 || interval > MaxMinInterval {
			return sourceBuildError(fail, "min_interval is %s; expected a positive duration up to %s", interval, MaxMinInterval)
		}
		source.MinInterval = interval
	}
	if file.Retry != nil {
		if err := applyRetry(&source.Retry, file.Retry, fail); err != nil {
			return err
		}
	}
	return nil
}

func applySourcePaths(source *Source, file fileSource, fail sourceBuildFail) error {
	if strings.TrimSpace(file.Records) != "" {
		parsed, err := ParseFieldPath(file.Records)
		if err != nil {
			_, wrapped := fail("records: %v", err)
			return wrapped
		}
		source.Records = parsed
	}
	return nil
}
