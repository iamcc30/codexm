package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/iamcc30/codexm/internal/monitor"
)

type sessionPage struct {
	Data     []monitor.Session `json:"data"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Pages    int               `json:"pages"`
	Facets   sessionFacets     `json:"facets"`
}

type sessionFacets struct {
	Profiles []string `json:"profiles"`
	Projects []string `json:"projects"`
	Statuses []string `json:"statuses"`
	Sources  []string `json:"sources"`
	Models   []string `json:"models"`
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page := positiveInt(query.Get("page"), 1)
	pageSize := positiveInt(query.Get("page_size"), 50)
	if pageSize > 200 {
		pageSize = 200
	}
	all := s.store.Snapshot().Sessions
	facets := buildFacets(all)
	search := strings.ToLower(strings.TrimSpace(query.Get("q")))
	filtered := make([]monitor.Session, 0, len(all))
	for _, item := range all {
		if search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{
			item.Title, item.Preview, item.ID, item.Profile, item.Project, item.Model, item.Source,
		}, " ")), search) {
			continue
		}
		if !matches(query.Get("profile"), item.Profile) ||
			!matches(query.Get("project"), item.Project) ||
			!matches(query.Get("status"), item.Status) ||
			!matches(query.Get("source"), item.Source) ||
			!matches(query.Get("model"), item.Model) {
			continue
		}
		switch query.Get("archived") {
		case "true":
			if !item.Archived {
				continue
			}
		case "false":
			if item.Archived {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	sortSessions(filtered, query.Get("sort"), query.Get("direction"))
	total := len(filtered)
	pages := (total + pageSize - 1) / pageSize
	if pages == 0 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}
	if start > total {
		start = total
	}
	writeJSON(w, sessionPage{
		Data: filtered[start:end], Total: total, Page: page,
		PageSize: pageSize, Pages: pages, Facets: facets,
	})
}

func buildFacets(items []monitor.Session) sessionFacets {
	profiles := map[string]bool{}
	projects := map[string]bool{}
	statuses := map[string]bool{}
	sources := map[string]bool{}
	models := map[string]bool{}
	for _, item := range items {
		addFacet(profiles, item.Profile)
		addFacet(projects, item.Project)
		addFacet(statuses, item.Status)
		addFacet(sources, item.Source)
		addFacet(models, item.Model)
	}
	return sessionFacets{
		Profiles: keys(profiles), Projects: keys(projects), Statuses: keys(statuses),
		Sources: keys(sources), Models: keys(models),
	}
}

func sortSessions(items []monitor.Session, key, direction string) {
	desc := direction != "asc"
	less := func(i, j int) bool {
		var result bool
		switch key {
		case "created":
			result = items[i].CreatedAt.Before(items[j].CreatedAt)
		case "title":
			result = strings.ToLower(items[i].Title) < strings.ToLower(items[j].Title)
		case "tokens":
			if items[i].TokenKnown != items[j].TokenKnown {
				return items[i].TokenKnown
			}
			result = items[i].Tokens < items[j].Tokens
		default:
			result = items[i].UpdatedAt.Before(items[j].UpdatedAt)
		}
		if desc {
			return !result && !sessionSortEqual(items[i], items[j], key)
		}
		return result
	}
	sort.SliceStable(items, less)
}

func sessionSortEqual(left, right monitor.Session, key string) bool {
	switch key {
	case "created":
		return left.CreatedAt.Equal(right.CreatedAt)
	case "title":
		return strings.EqualFold(left.Title, right.Title)
	case "tokens":
		return left.TokenKnown == right.TokenKnown && left.Tokens == right.Tokens
	default:
		return left.UpdatedAt.Equal(right.UpdatedAt)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func positiveInt(value string, fallback int) int {
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return fallback
	}
	return number
}

func matches(filter, value string) bool { return filter == "" || filter == value }

func addFacet(values map[string]bool, value string) {
	if value != "" {
		values[value] = true
	}
}

func keys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
