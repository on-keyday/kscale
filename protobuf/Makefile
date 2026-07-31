.PHONY: gen gen-options
# ksdk_options.proto は extension descriptor が必要なので、標準 protoc-gen-go で
# 先に生成し、自前 plugin が import できる状態にしてから本体を処理する。
gen-options:
	protoc --plugin=$(HOME)/go/bin/protoc-gen-go \
	       --go_out=. \
	       --go_opt=paths=source_relative \
	       --go_opt=Mproto/ksdkpb/ksdk_options.proto=github.com/on-keyday/kscale/protobuf/proto/ksdkpb \
	       proto/ksdkpb/ksdk_options.proto

gen: gen-options
	# プラグインのビルド (ksdk_options.pb.go を import するので gen-options 後)
	go build -o ./plugin/protoc-gen-ksdk ./plugin/main.go
	# すべてのprotoファイルを処理 (ksdk_options.proto は plugin 側で skip)
	protoc --plugin=./plugin/protoc-gen-ksdk \
	       --ksdk_out=. \
	       --ksdk_opt=paths=source_relative \
	       proto/*.proto proto/**/*.proto
	# バイナリの掃除
	rm ./plugin/protoc-gen-ksdk

gen-reference-impl:
    # 参照実装(protoc-gen-go)のコード生成
	protoc --plugin=$(HOME)/go/bin/protoc-gen-go \
	       --go_out=./reference \
	       --go_opt=paths=source_relative \
	       proto/*.proto proto/**/*.proto
	protoc --plugin=$(HOME)/go/bin/protoc-gen-go-grpc \
	       --go-grpc_out=./reference \
	       --go-grpc_opt=paths=source_relative \
	       proto/*.proto proto/**/*.proto
