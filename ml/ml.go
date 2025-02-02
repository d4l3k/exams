package ml

import (
	"log"
	"math/rand"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/d4l3k/docconv"
	"github.com/jbrukh/bayesian"
	"github.com/ubccsss/exams/examdb"
	"github.com/ubccsss/exams/util"
)

var yearRegex = regexp.MustCompile("(20|19)\\d{2}")

// Exam type
const (
	Midterm  bayesian.Class = "Midterm"
	Midterm1 bayesian.Class = "Midterm 1"
	Midterm2 bayesian.Class = "Midterm 2"
	Midterm3 bayesian.Class = "Midterm 3"
	Final    bayesian.Class = "Final"
	Quiz1    bayesian.Class = "Quiz 1"
	Quiz2    bayesian.Class = "Quiz 2"
	Quiz3    bayesian.Class = "Quiz 3"
	Quiz4    bayesian.Class = "Quiz 4"
	Quiz5    bayesian.Class = "Quiz 5"
)

// Sample or not
const (
	Sample bayesian.Class = "Sample"
	Real   bayesian.Class = ""
)

// Exam solution or not
const (
	Solution bayesian.Class = "Solution"
	Blank    bayesian.Class = ""
)

const (
	WinterTerm1 bayesian.Class = "W1"
	WinterTerm2 bayesian.Class = "W2"
	SummerTerm  bayesian.Class = "S"
)

// The class orders.
var (
	TypeClasses     = []bayesian.Class{Final, Midterm1, Midterm2, Midterm3, Midterm, Quiz1, Quiz2, Quiz3, Quiz4, Quiz5}
	SampleClasses   = []bayesian.Class{Sample, Real}
	SolutionClasses = []bayesian.Class{Solution, Blank}
	TermClasses     = []bayesian.Class{WinterTerm1, WinterTerm2, SummerTerm}
)

func typeClassFromLabel(label string) bayesian.Class {
	label = strings.ToLower(label)
	for _, t := range TypeClasses {
		if strings.Contains(label, strings.ToLower(string(t))) {
			return t
		}
	}
	return ""
}

func sampleClassFromLabel(label string) bayesian.Class {
	label = strings.ToLower(label)
	if strings.Contains(label, strings.ToLower(string(Sample))) {
		return Sample
	}
	return Real
}

func solutionClassFromLabel(label string) bayesian.Class {
	label = strings.ToLower(label)
	if strings.Contains(label, strings.ToLower(string(Solution))) {
		return Solution
	}
	return Blank
}

func termClassFromTerm(term string) bayesian.Class {
	term = strings.ToLower(term)
	for _, t := range TermClasses {
		if strings.Contains(term, strings.ToLower(string(t))) {
			return t
		}
	}
	return ""
}

// BayesianClassifier can classify a document into type, sample and solution
// classes.
type BayesianClassifier struct {
	TypeClassifier     *bayesian.Classifier
	SampleClassifier   *bayesian.Classifier
	SolutionClassifier *bayesian.Classifier
	TermClassifier     *bayesian.Classifier

	dir      string
	loadOnce sync.Once
}

// MakeDocumentClassifier trains a document classifier with all files in the DB.
func MakeDocumentClassifier() *BayesianClassifier {
	d := &BayesianClassifier{
		TypeClassifier:     bayesian.NewClassifier(TypeClasses...),
		SampleClassifier:   bayesian.NewClassifier(SampleClasses...),
		SolutionClassifier: bayesian.NewClassifier(SolutionClasses...),
		TermClassifier:     bayesian.NewClassifier(TermClasses...),
	}

	return d
}

const (
	TypeClassifierFile     = "type.classifier"
	SampleClassifierFile   = "sample.classifier"
	SolutionClassifierFile = "solution.classifier"
	TermClassifierFile     = "term.classifier"
)

func (d *BayesianClassifier) enforceLoaded() error {
	var err error
	d.loadOnce.Do(func() {
		d.TypeClassifier, err = bayesian.NewClassifierFromFile(path.Join(d.dir, TypeClassifierFile))
		if err != nil {
			return
		}
		d.SampleClassifier, err = bayesian.NewClassifierFromFile(path.Join(d.dir, SampleClassifierFile))
		if err != nil {
			return
		}
		d.SolutionClassifier, err = bayesian.NewClassifierFromFile(path.Join(d.dir, SolutionClassifierFile))
		if err != nil {
			return
		}
		d.TermClassifier, err = bayesian.NewClassifierFromFile(path.Join(d.dir, TermClassifierFile))
		if err != nil {
			return
		}
	})
	return err
}

// Load loads a classifier from a directory.
func (d *BayesianClassifier) Load(dir string) error {
	d.dir = dir

	return d.enforceLoaded()
}

// Save saves a classifier to a directory.
func (d BayesianClassifier) Save(dir string) error {
	if err := d.TypeClassifier.WriteToFile(path.Join(dir, TypeClassifierFile)); err != nil {
		return err
	}
	if err := d.SampleClassifier.WriteToFile(path.Join(dir, SampleClassifierFile)); err != nil {
		return err
	}
	if err := d.SolutionClassifier.WriteToFile(path.Join(dir, SolutionClassifierFile)); err != nil {
		return err
	}
	if err := d.TermClassifier.WriteToFile(path.Join(dir, TermClassifierFile)); err != nil {
		return err
	}
	return nil
}

type fileWords struct {
	*examdb.File
	words []string
}

func filesToWordBags(files []*examdb.File) (<-chan fileWords, <-chan error) {
	const workers = 8
	var wg sync.WaitGroup

	fChan := make(chan *examdb.File)
	bagChan := make(chan fileWords, 1)
	errChan := make(chan error, 1)

	go func() {
		for _, f := range files {
			fChan <- f
		}
		close(fChan)
	}()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			for f := range fChan {
				words, err := fileToWordBag(f)
				if err != nil {
					errChan <- err
					close(bagChan)
					close(errChan)
					break
				}
				bagChan <- fileWords{File: f, words: words}
			}
			wg.Done()
		}()
	}

	go func() {
		wg.Wait()
		close(bagChan)
		close(errChan)
	}()

	return bagChan, errChan
}

// Train trains a classifier from the database.
func (d BayesianClassifier) Train(db *examdb.Database) error {
	if err := d.enforceLoaded(); err != nil {
		return err
	}

	log.Println("Training document classifier...")
	documents := 0

	var files []*examdb.File

	db.Mu.RLock()
	for _, f := range db.Files {
		if f.NotAnExam || !f.HandClassified {
			continue
		}

		files = append(files, f)
	}
	db.Mu.RUnlock()

	for i := range files {
		j := rand.Intn(len(files))
		files[i], files[j] = files[j], files[i]
	}

	numTest := len(files) * 9 / 10
	trainFiles := files[:numTest]
	testFiles := files[numTest:]

	fileWordsChan, errChan := filesToWordBags(trainFiles)
	for f := range fileWordsChan {
		words := f.words
		if typeClass := typeClassFromLabel(f.Name); typeClass != "" {
			d.TypeClassifier.Learn(words, typeClass)
		}
		sampleClass := sampleClassFromLabel(f.Name)
		d.SampleClassifier.Learn(words, sampleClass)
		solutionClass := solutionClassFromLabel(f.Name)
		d.SolutionClassifier.Learn(words, solutionClass)
		if termClass := termClassFromTerm(f.Term); termClass != "" {
			d.TermClassifier.Learn(words, termClass)
		}

		documents++
		if documents%100 == 0 {
			log.Printf("... trained on %d", documents)
		}
	}

	for err := range errChan {
		if err != nil {
			return err
		}
	}

	/*
		for _, c := range []*bayesian.Classifier{d.TypeClassifier, d.SolutionClassifier, d.SampleClassifier, d.TermClassifier} {
			c.ConvertTermsFreqToTfIdf()
		}
	*/

	log.Printf("Finished training document classifier! %d documents.", documents)
	if err := d.Test(trainFiles, testFiles, db); err != nil {
		return err
	}
	return nil
}

// Classify returns the most likely labels for each function.
func (d BayesianClassifier) Classify(f *examdb.File) (map[string]string, error) {
	if err := d.enforceLoaded(); err != nil {
		return nil, err
	}

	words, err := fileToWordBag(f)
	if err != nil {
		return nil, err
	}

	typ, sample, solution, term := d.classifyWords(words)
	return map[string]string{
		"type":     string(typ),
		"sample":   string(sample),
		"solution": string(solution),
		"term":     string(term),
	}, nil
}

func (d BayesianClassifier) classifyWords(words []string) (bayesian.Class, bayesian.Class, bayesian.Class, bayesian.Class) {
	_, typeInx, _ := d.TypeClassifier.LogScores(words)
	_, sampleInx, _ := d.SampleClassifier.LogScores(words)
	_, solutionInx, _ := d.SolutionClassifier.LogScores(words)
	_, termInx, _ := d.TermClassifier.LogScores(words)
	return TypeClasses[typeInx],
		SampleClasses[sampleInx],
		SolutionClasses[solutionInx],
		TermClasses[termInx]
}

// Test computes the test error and prints it.
func (d BayesianClassifier) Test(trainFiles, testFiles []*examdb.File, db *examdb.Database) error {
	if err := d.runTest("TRAINING", trainFiles, db); err != nil {
		return err
	}
	if err := d.runTest("TEST", testFiles, db); err != nil {
		return err
	}
	return nil
}

func (d BayesianClassifier) runTest(label string, files []*examdb.File, db *examdb.Database) error {
	log.Printf("[%s] Testing document classifier...", label)

	documents := 0
	var typeRight, typeTotal, sampleRight, sampleTotal, solutionRight, solutionTotal, termRight, termTotal int

	fileWordsChan, errChan := filesToWordBags(files)
	for f := range fileWordsChan {
		words := f.words
		predType, predSample, predSolution, predTerm := d.classifyWords(words)

		if typeClass := typeClassFromLabel(f.Name); typeClass != "" {
			typeTotal++
			if predType == typeClass {
				typeRight++
			}
		}

		sampleClass := sampleClassFromLabel(f.Name)
		sampleTotal++
		if predSample == sampleClass {
			sampleRight++
		}

		solutionClass := solutionClassFromLabel(f.Name)
		solutionTotal++
		if predSolution == solutionClass {
			solutionRight++
		}

		if termClass := termClassFromTerm(f.Term); termClass != "" {
			termTotal++
			if predTerm == termClass {
				termRight++
			}
		}

		documents++
		if documents%100 == 0 {
			log.Printf("... tested on %d", documents)
		}
	}

	for err := range errChan {
		if err != nil {
			return err
		}
	}

	log.Printf(
		"[%s] Errors: Type %d/%d = %f, Sample %d/%d = %f, Solution %d/%d = %f, Term %d/%d = %f",
		label,
		typeRight, typeTotal, float64(typeRight)/float64(typeTotal),
		sampleRight, sampleTotal, float64(sampleRight)/float64(sampleTotal),
		solutionRight, solutionTotal, float64(solutionRight)/float64(solutionTotal),
		termRight, termTotal, float64(termRight)/float64(termTotal),
	)

	return nil
}

func fileContentWords(f *examdb.File) ([]string, map[string]string, error) {
	in, err := f.Reader()
	if err != nil {
		return nil, nil, err
	}
	defer in.Close()
	txt, meta, err := docconv.ConvertPDF(in)
	if err != nil {
		return nil, nil, err
	}
	return urlToWords(txt), meta, nil
}

func fileToWordBagMeta(f *examdb.File) ([]string, map[string]string, error) {
	words, meta, err := fileContentWords(f)
	if err != nil {
		return nil, nil, err
	}
	if len(f.Source) > 0 {
		words = append(words, urlToWords(strings.ToLower(f.Source))...)
	} else if len(f.Path) > 0 {
		words = append(words, urlToWords(strings.ToLower(path.Base(f.Path)))...)
	}

	var independentWords []string
	// Add n-grams
	//independentWords = append(independentWords, generateNGrams(words, 2)...)
	//independentWords = append(independentWords, generateNGrams(words, 3)...)

	// Split years out.
	independentWords = append(independentWords, splitDatesOut(words)...)

	words = append(words, independentWords...)

	return words, meta, nil
}

func fileToWordBag(f *examdb.File) ([]string, error) {
	words, _, err := fileToWordBagMeta(f)
	return words, err
}

func urlToWords(uri string) []string {
	if len(uri) == 0 {
		return nil
	}
	uri = strings.ToLower(uri)
	words := strings.FieldsFunc(uri, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	return words

	// We've disabled stopwords since it removes all numbers.
	/*
		txt := stopwords.CleanString(strings.Join(words, " "), "en", false)
		return strings.Split(txt, " ")
	*/
}

func splitDatesOut(words []string) []string {
	var additional []string
	for _, word := range words {
		match := util.YearRegexp.FindString(word)
		if len(match) > 0 && len(match) != len(word) {
			additional = append(additional, match)
			rest := strings.Split(word, match)
			for _, w := range rest {
				if len(w) > 0 {
					additional = append(additional, w)
				}
			}
		}
	}
	return additional
}

func generateNGrams(words []string, n int) []string {
	var out []string
	for i := range words {
		if i > len(words)-n {
			break
		}
		out = append(out, strings.Join(words[i:i+n], " "))
	}
	return out
}

// Classifier is the default classifier set by LoadOrTrainClassifier.
var (
	DefaultClassifier       *BayesianClassifier
	DefaultGoogleClassifier *GoogleClassifier
)

// LoadOrTrainClassifier loads or trains the classifier.
func LoadOrTrainClassifier(db *examdb.Database, classifierDir string) error {
	var err error
	DefaultGoogleClassifier, err = MakeGoogleClassifier()
	if err != nil {
		return err
	}

	if DefaultClassifier != nil {
		return nil
	}
	go func() {
		start := time.Now()
		log.Println("Loading classifier...")
		DefaultClassifier = MakeDocumentClassifier()
		if err := DefaultClassifier.Load(classifierDir); err != nil {
			log.Printf("Failed to load classifier: %s", err)
			if err := RetrainClassifier(db, classifierDir); err != nil {
				log.Printf("Failed to retrain classifier: %s", err)
			}
		}
		log.Printf("Loaded classifier. Took: %s", time.Since(start))
	}()

	return nil
}

// RetrainClassifier retrains classifier from db and saves it to disk.
func RetrainClassifier(db *examdb.Database, classifierDir string) error {
	c := MakeDocumentClassifier()
	if err := c.Train(db); err != nil {
		return err
	}
	if err := os.MkdirAll(classifierDir, 0755); err != nil {
		return err
	}
	if err := c.Save(classifierDir); err != nil {
		return err
	}
	DefaultClassifier = c
	return nil
}

// ExtractCourse returns the predicted courseID from the file source.
func ExtractCourse(db *examdb.Database, f *examdb.File) string {
	lowerPath := strings.ToLower(f.Source)
	var bestMatch string
	var bestMatchScore int
	for _, c := range db.Courses {
		for _, id := range c.AlternateIDs() {
			if !strings.Contains(lowerPath, id) {
				continue
			}
			score := len(id)
			if score > bestMatchScore {
				bestMatch = c.Code
				bestMatchScore = score
			}
		}
	}
	return bestMatch
}

// ExtractYear uses the text content of a file to infer the year it was from.
func ExtractYear(f *examdb.File) (int, string) {
	if f.Year > 0 {
		return f.Year, f.Term
	}

	/*
		lowerPath := strings.ToLower(f.Source)
		years := util.YearRegexp.FindAllString(lowerPath, -1)
		if len(years) > 0 {
			n, _ := strconv.Atoi(years[len(years)-1])
			return n
		}
	*/
	words, err := fileToWordBag(f)
	if err != nil {
		log.Println(err)
		return 0, examdb.TermUnknown
	}
	return ExtractYearFromWords(words)
}

type dateTemplate struct {
	tmpl     string
	hasMonth bool
}

var dateTemplates = []dateTemplate{
	{"January 2 2006", true},
	{"January 2st 2006", true},
	{"January 2nd 2006", true},
	{"January 2rd 2006", true},
	{"January 2th 2006", true},
	{"January 2006", true},
	{"2006", false},
	{"06", false},
}

// ExtractYearFromWords extracts the most frequent year found in the bag of
// words and returns it.
func ExtractYearFromWords(words []string) (int, string) {
	var templateWordCount []int
	for _, tmpl := range dateTemplates {
		templateWordCount = append(templateWordCount, len(strings.Split(tmpl.tmpl, " ")))
	}

	var foundYears []int
	var foundYearsHasMonth []int
	var foundTerms []string

	for i := 0; i < len(words); i++ {
		for j, tmpl := range dateTemplates {
			count := templateWordCount[j]
			if i+count > len(words) {
				continue
			}
			value := strings.Join(words[i:i+count], " ")
			t, err := time.Parse(tmpl.tmpl, value)
			if err != nil {
				continue
			}
			// Don't use dates in the future.
			if time.Now().Before(t) {
				continue
			}
			year, term := convertDateToYear(tmpl, t)
			if tmpl.hasMonth {
				foundYearsHasMonth = append(foundYearsHasMonth, year)
				foundTerms = append(foundTerms, term)
			} else {
				foundYears = append(foundYears, year)
			}
			i += count - 1
			break
		}
	}

	// Dates with months are inherently more accurate and avoids random duplicate
	// numbers.
	if len(foundYearsHasMonth) > 0 {
		foundYears = foundYearsHasMonth
	}

	return mostFrequentInt(foundYears, 0), mostFrequentString(foundTerms, examdb.TermUnknown)
}

// mostFrequentInt returns the most frequent integer. If there is a tie, it uses
// the one specified last. If there are no elements it returns fallback.
func mostFrequentInt(arr []int, fallback int) int {
	frequency := map[int]int{}
	lastIndex := map[int]int{}
	best := fallback

	for i, n := range arr {
		frequency[n]++
		lastIndex[n] = i
	}

	for year, freq := range frequency {
		bestFreq := frequency[best]
		if freq > bestFreq {
			best = year
		} else if freq == bestFreq {
			if lastIndex[year] > lastIndex[best] {
				best = year
			}
		}
	}

	return best
}

// mostFrequentString returns the most frequent string. If there is a tie, it uses
// the one specified last. If there are no elements it returns fallback.
func mostFrequentString(arr []string, fallback string) string {
	frequency := map[string]int{}
	lastIndex := map[string]int{}
	best := fallback

	for i, n := range arr {
		frequency[n]++
		lastIndex[n] = i
	}

	for year, freq := range frequency {
		bestFreq := frequency[best]
		if freq > bestFreq {
			best = year
		} else if freq == bestFreq {
			if lastIndex[year] > lastIndex[best] {
				best = year
			}
		}
	}

	return best
}

// convertDateToYear returns the year from t, unless the template hasMonth, in
// which it checks if it's during January to April and subtracts 1 from the year
// if that's the case.
func convertDateToYear(tmpl dateTemplate, t time.Time) (int, string) {
	if tmpl.hasMonth {
		var term string
		m := t.Month()
		if m >= time.January && m <= time.April {
			term = examdb.TermW2
		} else if m >= time.May && m <= time.August {
			term = examdb.TermS
		} else if m >= time.September && m <= time.December {
			term = examdb.TermW1
		}

		if term == examdb.TermW2 {
			return t.Year() - 1, term
		}
		return t.Year(), term
	}
	return t.Year(), examdb.TermUnknown
}

// ConvertDateToYearTerm returns the year and the term from t.
func ConvertDateToYearTerm(t time.Time) (int, string) {
	return convertDateToYear(dateTemplate{hasMonth: true}, t)
}
