package ruler

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/prequel-dev/prequel-compiler/pkg/ast"
	"github.com/prequel-dev/prequel-compiler/pkg/parser"
	"github.com/prequel-dev/prequel-compiler/pkg/pqerr"
	"github.com/rs/zerolog/log"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

var (
	ErrInvalidType     = errors.New("invalid type")
	ErrDuplicateRuleId = errors.New("duplicate rule id")
)

var (
	packageName = "cre-rules"
	tagsDir     = "tags"
	tagsYaml    = "tags.yaml"
	catsYaml    = "categories.yaml"
)

type BuildCmd struct {
	InPath  string `name:"path" short:"p" help:"Path to read rules" default:"rules"`
	OutPath string `name:"out" short:"o" help:"Optional path to write files; default curdir"`
	Version string `name:"vers" short:"v" help:"Optional semantic version override"`
}

func RunBuild(inPath, outPath, vers string) error {

	if outPath == "" {
		var err error
		outPath, err = os.Getwd()
		if err != nil {
			log.Error().Err(err).Msg("Fail os.Getwd()")
			return err
		}
	}

	if vers == "" {
		vers = Semver()
	}

	if !strings.HasPrefix(vers, "v") {
		vers = "v" + vers
	}

	if !semver.IsValid(vers) {
		return fmt.Errorf("invalid semver: %s", vers)
	}

	if outPath != "" {
		if err := os.MkdirAll(outPath, 0755); err != nil {
			log.Error().Err(err).Msg("Fail mkdir all")
			return err
		}
	}

	if err := _build(vers, inPath, outPath, packageName); err != nil {
		return err
	}

	return nil
}

func processTags(inPath string) (tagsT, error) {
	var (
		tagsData          []byte
		categoriesData    []byte
		tagsSection       RuleIncludeT
		categoriesSection RuleIncludeT
		tags              = make(tagsT)
		err               error
	)

	tagsData, err = os.ReadFile(filepath.Join(inPath, tagsDir, tagsYaml))
	if err != nil {
		log.Error().Err(err).Msg("Fail read tags")
		return nil, err
	}

	if err := yaml.Unmarshal(tagsData, &tagsSection); err != nil {
		log.Error().Err(err).Msg("Fail unmarshal tags")
		return nil, err
	}

	categoriesData, err = os.ReadFile(filepath.Join(inPath, tagsDir, catsYaml))
	if err != nil {
		log.Error().Err(err).Msg("Fail read categories")
		return nil, err
	}

	if err := yaml.Unmarshal(categoriesData, &categoriesSection); err != nil {
		log.Error().Err(err).Msg("Fail unmarshal categories")
		return nil, err
	}

	if err := validateTagsFields(tagsSection, tags); err != nil {
		log.Error().Err(err).Str("file", filepath.Join(inPath, tagsDir, tagsYaml)).Msg("Fail validate tags")
		return nil, err
	}

	if err := validateCategoriesFields(categoriesSection, tags); err != nil {
		log.Error().Err(err).Str("file", filepath.Join(inPath, tagsDir, catsYaml)).Msg("Fail validate categories")
		return nil, err
	}

	return tags, nil
}

func processRules(path string, ruleDupes, termDupes dupesT, tags tagsT) (*parser.RulesT, error) {

	var (
		rulesData []byte
		allRules  = &parser.RulesT{
			Rules:  make([]parser.ParseRuleT, 0),
			TermsT: make(map[string]parser.ParseTermT),
		}
		err error
	)

	yamls, err := os.ReadDir(path)
	if err != nil {
		log.Error().Err(err).Msg("Fail read rules")
		return nil, err
	}

	for _, y := range yamls {

		var (
			rules parser.RulesT
			f     = filepath.Join(path, y.Name())
		)

		log.Debug().
			Str("file", y.Name()).
			Msg("Processing rule")

		if !strings.HasSuffix(y.Name(), ".yaml") {
			continue
		}

		rulesData, err = os.ReadFile(f)
		if err != nil {
			log.Error().Err(err).Msg("Fail read rules")
			return nil, err
		}

		if err = compile(rulesData); err != nil {
			pqerr.WithFile(err, f)
			return nil, err
		}

		if err := yaml.Unmarshal(rulesData, &rules); err != nil {
			log.Error().Err(err).Msg("Fail unmarshal rules")
			return nil, err
		}

		if err = validateRules(rules, ruleDupes, termDupes, tags); err != nil {
			log.Error().Err(err).Str("file", filepath.Join(path, y.Name())).Msg("Fail validate rules")
			return nil, err
		}

		log.Trace().Any("rules", rules).Msg("Rules")

		for _, rule := range rules.Rules {
			rule.Metadata.Hash, err = hashRule(rule)
			if err != nil {
				return nil, err
			}

			log.Info().
				Str("hash", rule.Metadata.Hash).
				Str("id", rule.Cre.Id).
				Msg("Rule")

			allRules.Rules = append(allRules.Rules, rule)
		}

		for key, term := range rules.TermsT {
			allRules.TermsT[key] = term
		}
	}

	return allRules, nil
}

func _build(vers, inPath, outPath, packageName string) error {

	var (
		allRules  = make(map[string]parser.ParseRuleT)
		allTerms  = make(map[string]parser.ParseTermT)
		ruleDupes = make(dupesT)
		termDupes = make(dupesT)
		tags      tagsT
		err       error
	)

	log.Info().Str("vers", vers).Str("outPath", outPath).Msg("Building")

	if tags, err = processTags(inPath); err != nil {
		return err
	}

	log.Debug().Any("tags", tags).Msg("Tags")

	cres, err := os.ReadDir(inPath)
	if err != nil {
		log.Error().Err(err).Msg("Fail read rules dir")
		return err
	}

	for _, e := range cres {

		var (
			r   *parser.RulesT
			err error
		)

		if !e.IsDir() {
			log.Debug().Str("file", e.Name()).Msg("Skipping")
			continue
		}

		if !strings.HasPrefix(e.Name(), "cre-") {
			log.Debug().Str("file", e.Name()).Msg("Skipping")
			continue
		}

		log.Debug().Str("file", e.Name()).Msg("Processing target")

		if r, err = processRules(filepath.Join(inPath, e.Name()), ruleDupes, termDupes, tags); err != nil {
			log.Error().Err(err).Msg("Fail process rules")
			return err
		}

		for _, rule := range r.Rules {
			allRules[rule.Cre.Id] = rule
		}

		for key, term := range r.TermsT {
			allTerms[key] = term
		}
	}

	doc, err := generateDocument(allRules, allTerms)
	if err != nil {
		return err
	}

	// Validate final document compiles
	if err = compileCombinedDoc(doc); err != nil {
		log.Error().Err(err).Msg("Fail compile")
		return err
	}

	fileName := makeFilename(packageName, vers)
	fullPath := filepath.Join(outPath, fileName)

	if err = writeFile(fullPath, doc); err != nil {
		return err
	}

	fmt.Printf("Wrote file: %s\n", fileName)

	return nil
}

func compile(data []byte) error {

	var (
		err error
	)

	if _, err = ast.Build(data); err != nil {
		return err
	}

	return nil
}

func compileCombinedDoc(data []byte) error {

	var (
		rules *parser.RulesT
		err   error
	)

	if rules, err = parser.Read(bytes.NewReader(data)); err != nil {
		return err
	}

	if _, err := parser.ParseRules(rules); err != nil {
		return err
	}

	return nil
}

func writeFile(fn string, data []byte) error {
	fh, err := os.OpenFile(fn, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	if _, err = fh.Write(data); err != nil {
		fh.Close()
		return err
	}

	return fh.Close()
}

func makeFilename(name, vers string) string {
	name = strings.TrimSuffix(name, ".yaml")
	vers = strings.TrimPrefix(vers, "v")

	return fmt.Sprintf("%s.%s.yaml", name, vers)
}

// Convert to document per section

func generateDocument(rules map[string]parser.ParseRuleT, terms map[string]parser.ParseTermT) ([]byte, error) {

	// Gather keys to produce consistent order output
	ruleKeys := make([]string, 0, len(rules))
	for k := range rules {
		ruleKeys = append(ruleKeys, k)
	}
	sort.Strings(ruleKeys)

	termKeys := make([]string, 0, len(terms))
	for k := range terms {
		termKeys = append(termKeys, k)
	}
	sort.Strings(termKeys)

	var buf bytes.Buffer

	doc := parser.RulesT{
		Rules:  make([]parser.ParseRuleT, 0),
		TermsT: make(map[string]parser.ParseTermT),
	}

	for _, k := range ruleKeys {
		log.Debug().Any("rule", rules[k]).Msg("Adding rule")
		doc.Rules = append(doc.Rules, rules[k])
	}

	for _, k := range termKeys {
		log.Debug().Any("term", terms[k]).Msg("Adding term")
		doc.TermsT[k] = terms[k]
	}

	y, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}

	buf.Write(y)

	return buf.Bytes(), nil
}
