package main

import (
	"context"
	"flag"
	"github.com/loudermachine/parallelize/internal/worker"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go/token"
	"golang.org/x/tools/go/packages"
	"os"
	"strings"
	"sync"
)

const mode packages.LoadMode = packages.NeedName |
	packages.NeedTypes |
	packages.NeedSyntax |
	packages.NeedTypesInfo

func main() {
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()
	zerolog.DefaultContextLogger = &logger

	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal().Msg("Expecting a single argument: directory of module.")
	}

	fset := token.NewFileSet()
	cfg := &packages.Config{Fset: fset, Mode: mode, Dir: flag.Args()[0], Tests: true}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		log.Fatal().Msgf("Failed to load packages: %v.", err)
	}

	var wg sync.WaitGroup
	for _, pkg := range pkgs {
		ctx := context.Background()
		lg := log.Ctx(ctx)
		lg.UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Str("pkg-name", pkg.ID)
		})
		if !strings.HasSuffix(pkg.ID, ".test]") {
			lg.Trace().Msg("Skipping package due to not being the test package")
			continue
		}
		w := worker.New(pkg, fset)
		wg.Add(1)
		go func() {
			lg.Trace().Msg("Launching worker.")
			w.Run(ctx)
			wg.Done()
			lg.Trace().Msg("Worker is done.")
		}()
	}
	wg.Wait()
	log.Info().Msg("Workload finished.")
}
