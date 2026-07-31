package wkt

// ToCLIOutput は CLI 上で Empty レスポンスを成功表示するためのヘルパー。
// agent.ResponseDTO 互換のため method 必須。Empty に表示する情報はないので
// 単純な "ok" を返す。
func (*Empty) ToCLIOutput() string {
	return "ok"
}

// ProtoTypeName / EncodeAdminResponseBody は agent.ResponseDTO typed wire
// path 用 (A4 phase 21)。Empty は body 空。
func (*Empty) ProtoTypeName() string                    { return "Empty" }
func (*Empty) EncodeAdminResponseBody() ([]byte, error) { return nil, nil }
