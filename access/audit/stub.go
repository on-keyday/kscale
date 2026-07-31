package audit

import (
	"fmt"
	"io"
	"strings"
	"time"
)

type stubAudit struct {
	Writer io.Writer
}

var _ Audit = (*stubAudit)(nil)

func (a *stubAudit) RecordPolicyStart(t AuditDecisionType, record *PolicyStartRecord) {
	if a.Writer != nil {
		fmt.Fprintf(a.Writer, "Policy Start [%s] at %s: %s\n", t, time.Now().Format(time.RFC3339), record.PolicyName)
	}
}

func (a *stubAudit) RecordPolicyDecision(t AuditDecisionType, decision Decision) {
	if a.Writer != nil {
		decisionString := decision.String()
		fmt.Fprintf(a.Writer, "Policy Decision [%s]: %s\n", t, decisionString)
	}
}

func (a *stubAudit) RecordAttributeResolve(record *ResolveRecord) {
	if a.Writer != nil {
		fmt.Fprintf(a.Writer, "Attribute Resolved [%s]: Context=%s, Attribute=%s, Resolved=%v, Err=%v\n",
			record.PolicyName,
			record.Context,
			strings.Join(record.Attribute, "."),
			record.Resolved,
			record.Err,
		)
	}
}

func (a *stubAudit) RecordUsedValue(record *UsedValueRecord) {
	if a.Writer != nil {
		fmt.Fprintf(a.Writer, "Used Value [%s]: Field=%s, Value=%v\n",
			record.PolicyName,
			record.Field,
			record.Value,
		)
	}
}

func (a *stubAudit) RecordCompareResult(record *CompareResultRecord) {
	if a.Writer != nil {
		fmt.Fprintf(a.Writer, "Compare Result: Condition=%s, Target=%v, Result=%v, Err=%v\n",
			record.Condition,
			record.Target,
			record.Result,
			record.Err,
		)
	}
}

func (a *stubAudit) RecordRemoteShellStart(command string, args []string, ptyEnabled bool) {
	if a.Writer != nil {
		fmt.Fprintf(a.Writer, "Remote Shell Start: Command=%s, Args=%v, PTY Enabled=%v at %s\n", command, args, ptyEnabled, time.Now().Format(time.RFC3339))
	}
}

func (a *stubAudit) RecordRemoteShellStdin(data []byte) {
	if a.Writer != nil {
		fmt.Fprintf(a.Writer, "Remote Shell Stdin: Data=%q\n", string(data))
	}
}

func (a *stubAudit) RecordRemoteShellStdout(data []byte) {
	if a.Writer != nil {
		fmt.Fprintf(a.Writer, "%s: Remote Shell Stdout: Data=%q\n", time.Now().Format(time.RFC3339), string(data))
	}
}

func (a *stubAudit) RecordRemoteShellStderr(data []byte) {
	if a.Writer != nil {
		fmt.Fprintf(a.Writer, "%s: Remote Shell Stderr: Data=%q\n", time.Now().Format(time.RFC3339), string(data))
	}
}

func (a *stubAudit) RecordRemoteShellExit(code error) {
	if a.Writer != nil {
		fmt.Fprintf(a.Writer, "%s: Remote Shell Exit: Code=%v\n", time.Now().Format(time.RFC3339), code)
	}
}

// NewLoggerAudit creates a stub audit implementation that writes audit records to the provided writer.
func NewLoggerAudit(w io.Writer) Audit {
	return &stubAudit{Writer: w}
}
