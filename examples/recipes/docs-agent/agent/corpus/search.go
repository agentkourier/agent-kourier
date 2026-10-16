package corpus

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// Limits on a search.
const (
	// DefaultLimit is the number of pages Search returns when the caller names none.
	DefaultLimit = 5
	// MaxLimit is the most pages Search returns.
	MaxLimit = 10
	// maxQueryBytes and maxQueryTerms bound how much of a query is used; the rest is ignored.
	maxQueryBytes = 1024
	maxQueryTerms = 16

	// BM25 parameters, and how much a word in a heading counts against one in the body, and a word in the page
	// title against the best section.
	bm25K1       = 1.2
	bm25B        = 0.75
	headingBoost = 3.0
	titleBoost   = 0.5

	excerptBytes  = 300
	excerptLeadIn = 80
)

// Hit is one page that matches a query, with the section of it that matches best.
type Hit struct {
	// Path is the page's path in the corpus.
	Path string
	// Title is the page's title.
	Title string
	// Heading and Anchor name the best-matching section; both are empty if it is the text before the first
	// heading.
	Heading string
	Anchor  string
	// Excerpt is a short single-line passage of that section around the first matching word.
	Excerpt string
	// Score is the rank value; the larger, the better. It is comparable only within one result.
	Score float64
}

// indexedSection is a section as the ranking sees it.
type indexedSection struct {
	// tf is the weighted count of each token: 1 for a body word, headingBoost for a word of the heading.
	tf map[string]float64
	// length is the number of tokens, unweighted, for length normalisation.
	length int
	// plain is the section's text as a reader sees it, with a link shown as its label, for the excerpt.
	plain string
}

func indexSection(s section) indexedSection {
	is := indexedSection{tf: map[string]float64{}, plain: mdLink.ReplaceAllString(s.text, "$1")}
	scan(is.plain, func(tok string, _, _ int) {
		is.tf[tok]++
		is.length++
	})
	scan(s.heading, func(tok string, _, _ int) {
		is.tf[tok] += headingBoost
		is.length++
	})
	return is
}

// ranking holds the statistics BM25 needs over all sections of the corpus.
type ranking struct {
	sections int
	avgLen   float64
	df       map[string]int
}

func newRanking(pages []*page) ranking {
	r := ranking{df: map[string]int{}}
	var total int
	for _, p := range pages {
		for _, s := range p.secs {
			r.sections++
			total += s.length
			for tok := range s.tf {
				r.df[tok]++
			}
		}
	}
	r.avgLen = math.Max(float64(total)/float64(max(r.sections, 1)), 1)
	return r
}

func (r ranking) idf(tok string) float64 {
	df := float64(r.df[tok])
	return math.Log(1 + (float64(r.sections)-df+0.5)/(df+0.5))
}

// Search ranks the pages of the corpus against the words of query and returns at most limit of them, best first.
// limit <= 0 means DefaultLimit, and a limit over MaxLimit is MaxLimit. Each page appears once, with the section
// that scores best for it.
//
// The ranking is BM25 over sections, with a word of a heading counting three times a word of the body, plus half
// the weight of the query words that are in the page's title. Words are lower-cased and stemmed, and a camel-case
// identifier matches both whole and in parts. Equal scores are ordered by path, so the same corpus and query
// always give the same list. A query with no match gives an empty list; a query with no searchable word at all
// gives ErrEmptyQuery.
func (ix *Index) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case limit <= 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		limit = MaxLimit
	}
	terms := queryTerms(query)
	if len(terms) == 0 {
		return nil, ErrEmptyQuery
	}
	idf := make([]float64, len(terms))
	for i, t := range terms {
		idf[i] = ix.ranking.idf(t)
	}

	type scored struct {
		page    *page
		section int
		score   float64
	}
	var found []scored
	for _, p := range ix.pages {
		best, bestScore := -1, 0.0
		for i, s := range p.secs {
			var score float64
			for j, t := range terms {
				tf := s.tf[t]
				if tf == 0 {
					continue
				}
				norm := 1 - bm25B + bm25B*float64(s.length)/ix.ranking.avgLen
				score += idf[j] * tf * (bm25K1 + 1) / (tf + bm25K1*norm)
			}
			if score > bestScore {
				best, bestScore = i, score
			}
		}
		if best < 0 {
			continue
		}
		for j, t := range terms {
			if p.titleTokens[t] {
				bestScore += titleBoost * idf[j]
			}
		}
		found = append(found, scored{p, best, bestScore})
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].score != found[j].score {
			return found[i].score > found[j].score
		}
		return found[i].page.path < found[j].page.path
	})
	if len(found) > limit {
		found = found[:limit]
	}
	hits := make([]Hit, len(found))
	for i, f := range found {
		sec := f.page.sections[f.section]
		hits[i] = Hit{
			Path: f.page.path, Title: f.page.title, Heading: sec.heading, Anchor: sec.anchor,
			Excerpt: excerpt(f.page.secs[f.section].plain, terms), Score: f.score,
		}
	}
	return hits, nil
}

// queryTerms returns the distinct searchable tokens of query, the first maxQueryTerms in order of appearance,
// sorted so that scores are summed in the same order every time.
func queryTerms(query string) []string {
	if len(query) > maxQueryBytes {
		query = strings.ToValidUTF8(query[:maxQueryBytes], "")
	}
	seen := map[string]bool{}
	var terms []string
	scan(query, func(tok string, _, _ int) {
		if !seen[tok] && len(terms) < maxQueryTerms {
			seen[tok] = true
			terms = append(terms, tok)
		}
	})
	sort.Strings(terms)
	return terms
}

// excerpt returns a one-line passage of text of about excerptBytes bytes, around its first word that is one of
// terms, or from its start if none is. A cut at either end is marked with an ellipsis.
func excerpt(text string, terms []string) string {
	want := map[string]bool{}
	for _, t := range terms {
		want[t] = true
	}
	at := -1
	scan(text, func(tok string, start, _ int) {
		if at < 0 && want[tok] {
			at = start
		}
	})
	from := 0
	if at > excerptLeadIn {
		from = at - excerptLeadIn
		for from < len(text) && !utf8.RuneStart(text[from]) {
			from++
		}
		// Start at a word: move on to just after the next white space, if one is near.
		if i := strings.IndexAny(text[from:], " \n\t"); i >= 0 && i < 40 {
			from += i + 1
		}
	}
	to := len(text)
	if to-from > excerptBytes {
		to = from + excerptBytes
		for to > from && !utf8.RuneStart(text[to]) {
			to--
		}
		if i := strings.LastIndexAny(text[from:to], " \n\t"); i > excerptBytes/2 {
			to = from + i
		}
	}
	out := strings.Join(strings.Fields(text[from:to]), " ")
	if from > 0 {
		out = "…" + out
	}
	if to < len(text) {
		out += "…"
	}
	return out
}
