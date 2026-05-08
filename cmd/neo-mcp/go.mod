module github.com/ensamblatec/neoanvil/cmd/neo-mcp

go 1.26.1

require (
	github.com/ensamblatec/neoanvil/pkg/astx v0.0.0
	github.com/ensamblatec/neoanvil/pkg/mctx v0.0.0
	github.com/ensamblatec/neoanvil/pkg/memx v0.0.0
	github.com/ensamblatec/neoanvil/pkg/rag v0.0.0
	github.com/ensamblatec/neoanvil/pkg/wasmx v0.0.0
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/smacker/go-tree-sitter v0.0.0-20240827094217-dd81d9e9be82 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/tetratelabs/wazero v1.11.0 // indirect
	go.etcd.io/bbolt v1.4.3 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

replace (
	github.com/ensamblatec/neoanvil/pkg/astx => ../../pkg/astx
	github.com/ensamblatec/neoanvil/pkg/mctx => ../../pkg/mctx
	github.com/ensamblatec/neoanvil/pkg/memx => ../../pkg/memx
	github.com/ensamblatec/neoanvil/pkg/rag => ../../pkg/rag
	github.com/ensamblatec/neoanvil/pkg/wasmx => ../../pkg/wasmx
)
