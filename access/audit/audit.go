package audit

import (
	"fmt"
)

type ResolveRecord struct {
	PolicyName string
	Context    string
	Attribute  []string
	Resolved   any
	Err        error
}

type UsedValueRecord struct {
	PolicyName string
	Field      string
	Value      any
}

type CompareResultRecord struct {
	Condition string
	Target    any
	Result    bool
	Err       error
}

type PolicyStartRecord struct {
	PolicyName string
}

type AuditDecisionType string

const (
	DecisionTypeAllow AuditDecisionType = "allow"
	DecisionTypeDeny  AuditDecisionType = "deny"
)

type Decision interface {
	fmt.Stringer
}

type PolicyAudit interface {
	RecordPolicyStart(t AuditDecisionType, record *PolicyStartRecord)
	RecordPolicyDecision(t AuditDecisionType, decision Decision)
	RecordAttributeResolve(record *ResolveRecord)
	RecordUsedValue(record *UsedValueRecord)
	RecordCompareResult(record *CompareResultRecord)
}

type RemoteShellAudit interface {
	RecordRemoteShellStart(command string, args []string, ptyEnabled bool)
	RecordRemoteShellStdin(data []byte)
	RecordRemoteShellStdout(data []byte)
	RecordRemoteShellStderr(data []byte)
	RecordRemoteShellExit(code error)
}

type Audit interface {
	PolicyAudit
	RemoteShellAudit
}
