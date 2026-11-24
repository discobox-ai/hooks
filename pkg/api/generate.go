package api

//go:generate go tool ogen --config gen/ogen.yml --target gen --clean --package hookapigen openapi/hooks.yaml
//go:generate go run ../../internal/cmd/genmodelaliases -schema-dir ./openapi/components/model -out ./model/aliases_gen.go -gen-import github.com/discobox-ai/hooks/pkg/api/gen -gen-package hookapigen
