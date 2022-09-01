package run

import (
	"time"

	"github.com/pkg/errors"
	"golang.stackrox.io/kube-linter/internal/version"
	"golang.stackrox.io/kube-linter/pkg/checkregistry"
	"golang.stackrox.io/kube-linter/pkg/config"
	"golang.stackrox.io/kube-linter/pkg/diagnostic"
	"golang.stackrox.io/kube-linter/pkg/ignore"
	"golang.stackrox.io/kube-linter/pkg/instantiatedcheck"
	"golang.stackrox.io/kube-linter/pkg/lintcontext"
)

// Reasonable default, could potentially make this configurable in the future
const maxConcurrentLints = 8

// CheckStatus is enum type.
type CheckStatus string

const (
	// ChecksPassed means no lint errors found.
	ChecksPassed CheckStatus = "Passed"
	// ChecksFailed means lint errors were found.
	ChecksFailed CheckStatus = "Failed"
)

// Result represents the result from a run of the linter.
type Result struct {
	Checks  []config.Check
	Reports []diagnostic.WithContext
	Summary Summary
}

// Summary holds information about the linter run overall.
type Summary struct {
	ChecksStatus      CheckStatus
	CheckEndTime      time.Time
	KubeLinterVersion string
}

// Run runs the linter on the given context, with the given config.
func Run(lintCtxs []lintcontext.LintContext, registry checkregistry.CheckRegistry, checks []string) (Result, error) {
	var result Result

	instantiatedChecks := make([]*instantiatedcheck.InstantiatedCheck, 0, len(checks))
	for _, checkName := range checks {
		instantiatedCheck := registry.Load(checkName)
		if instantiatedCheck == nil {
			return Result{}, errors.Errorf("check %q not found", checkName)
		}
		instantiatedChecks = append(instantiatedChecks, instantiatedCheck)
		result.Checks = append(result.Checks, instantiatedCheck.Spec)
	}

	var results = make(chan diagnostic.WithContext)
	defer close(results)
	var limit = make(chan struct{}, maxConcurrentLints)
	var done = make(chan struct{})

	for _, lintCtx := range lintCtxs {
		for _, obj := range lintCtx.Objects() {
			for _, check := range instantiatedChecks {
				go func(lintCtx lintcontext.LintContext, obj lintcontext.Object, check *instantiatedcheck.InstantiatedCheck) {
					// Block waiting on a spot in the channel
					limit <- struct{}{}
					defer func() { <-limit }()

					if !check.Matcher.Matches(obj.K8sObject.GetObjectKind().GroupVersionKind()) {
						return
					}
					if ignore.ObjectForCheck(obj.K8sObject.GetAnnotations(), check.Spec.Name) {
						return
					}

					diagnostics := check.Func(lintCtx, obj)
					for _, d := range diagnostics {
						results <- diagnostic.WithContext{
							Diagnostic:  d,
							Check:       check.Spec.Name,
							Remediation: check.Spec.Remediation,
							Object:      obj,
						}
					}
				}(lintCtx, obj, check)
			}
		}
	}

	go func() {
		for i := 0; i < maxConcurrentLints; i++ {
			// wait until we can fill the whole channel, meaning the go routines are done
			limit <- struct{}{}
		}
		done <- struct{}{}
	}()

chanLoop:
	for {
		select {
		case diag := <-results:
			result.Reports = append(result.Reports, diag)
		case <-done:
			break chanLoop
		}
	}

	if len(result.Reports) > 0 {
		result.Summary.ChecksStatus = ChecksFailed
	} else {
		result.Summary.ChecksStatus = ChecksPassed
	}
	result.Summary.CheckEndTime = time.Now().UTC()
	result.Summary.KubeLinterVersion = version.Get()

	return result, nil
}
