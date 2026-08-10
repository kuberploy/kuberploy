package main

import (
	"fmt"
	"os"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: validate-manifest <schema.json> <manifest.json>")
		os.Exit(64)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	schema, err := compiler.Compile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile release manifest schema: %v\n", err)
		os.Exit(1)
	}
	file, err := os.Open(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "open release manifest: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()
	instance, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode release manifest: %v\n", err)
		os.Exit(1)
	}
	if err = schema.Validate(instance); err != nil {
		fmt.Fprintf(os.Stderr, "release manifest does not match JSON Schema: %v\n", err)
		os.Exit(1)
	}
}
