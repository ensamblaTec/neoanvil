module github.com/ensamblatec/neoanvil/pkg/rag

go 1.26.1

require (
	github.com/fsnotify/fsnotify v1.9.0
	go.etcd.io/bbolt v1.4.3
	github.com/ensamblatec/neoanvil/pkg/memx v0.0.0
	github.com/ensamblatec/neoanvil/pkg/tensorx v0.0.0
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

replace (
	github.com/ensamblatec/neoanvil/pkg/memx => ../memx
	github.com/ensamblatec/neoanvil/pkg/tensorx => ../tensorx
)
