package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/fields"
	"cms/internal/jobs"
	"cms/internal/rbac"
	"cms/internal/richtext"
)

var ErrTermNotFound = errors.New("term not found")

type TermInput struct {
	Taxonomy    string
	Name        string
	Slug        string
	Description string
	ParentID    *uuid.UUID
	Fields      json.RawMessage
}

func (s *Service) Terms(ctx context.Context, taxonomy string) ([]dbq.ListTermsRow, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return nil, err
	}
	return s.DB.Q.ListTerms(ctx, taxonomy)
}

func (s *Service) Term(ctx context.Context, id uuid.UUID) (dbq.Term, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return dbq.Term{}, err
	}
	t, err := s.DB.Q.GetTerm(ctx, id)
	if db.IsNotFound(err) {
		return t, ErrTermNotFound
	}
	return t, err
}

func (s *Service) cleanTerm(in TermInput, a *actor.Actor) (TermInput, json.RawMessage, error) {
	tax := s.Site().Taxonomies[in.Taxonomy]
	if tax == nil {
		return in, nil, fmt.Errorf("unknown taxonomy %q", in.Taxonomy)
	}
	errs := fields.Errors{}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		errs["name"] = "is required"
	}
	if len([]rune(in.Name)) > 200 {
		errs["name"] = "must be at most 200 characters"
	}
	in.Slug = richtext.Slugify(firstNonEmpty(in.Slug, in.Name))
	if in.Slug == "" {
		errs["slug"] = "is required"
	}
	if !tax.Hierarchical {
		in.ParentID = nil
	}
	fj, _, err := fields.Validate(s.Site().TermFields(in.Taxonomy), in.Fields,
		fields.Context{CanUse: func(c string) bool { return a.Can(rbac.Cap(c)) }})
	if err != nil {
		mergeErrs(errs, err, "fields.")
	}
	if len(errs) > 0 {
		return in, nil, errs
	}
	return in, fj, nil
}

func (s *Service) CreateTerm(ctx context.Context, in TermInput) (dbq.Term, error) {
	a, err := actor.Require(ctx, rbac.TermsManage)
	if err != nil {
		return dbq.Term{}, err
	}
	in, fj, err := s.cleanTerm(in, a)
	if err != nil {
		return dbq.Term{}, err
	}
	var out dbq.Term
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		t, err := q.CreateTerm(ctx, dbq.CreateTermParams{ID: uuid.Must(uuid.NewV7()), Taxonomy: in.Taxonomy,
			ParentID: in.ParentID, Slug: in.Slug, Name: in.Name, Description: strings.TrimSpace(in.Description), Fields: fj})
		if db.IsUniqueViolation(err, "") {
			return fields.Errors{"slug": "is already used in this taxonomy"}
		}
		if err != nil {
			return err
		}
		out = t
		return audit.Record(ctx, q, audit.Event{Action: "term.create", ResourceType: "term", ResourceID: t.ID.String(),
			Diff: map[string]any{"taxonomy": in.Taxonomy, "slug": in.Slug}})
	})
	return out, err
}

func (s *Service) UpdateTerm(ctx context.Context, id uuid.UUID, in TermInput) (dbq.Term, error) {
	a, err := actor.Require(ctx, rbac.TermsManage)
	if err != nil {
		return dbq.Term{}, err
	}
	var out dbq.Term
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		old, err := q.GetTerm(ctx, id)
		if err != nil {
			return ErrTermNotFound
		}
		in.Taxonomy = old.Taxonomy
		in, fj, err := s.cleanTerm(in, a)
		if err != nil {
			return err
		}
		if in.ParentID != nil && *in.ParentID == id {
			return fields.Errors{"parent_id": "cannot be the term itself"}
		}
		t, err := q.UpdateTerm(ctx, dbq.UpdateTermParams{ID: id, ParentID: in.ParentID, Slug: in.Slug, Name: in.Name,
			Description: strings.TrimSpace(in.Description), Fields: fj})
		if db.IsUniqueViolation(err, "") {
			return fields.Errors{"slug": "is already used in this taxonomy"}
		}
		if err != nil {
			return err
		}
		out = t
		if old.Slug != t.Slug {
			if p := s.Site().Taxonomies[t.Taxonomy].Pattern(); p != nil {
				from := p.Build(map[string]string{"slug": old.Slug, "path": old.Slug})
				to := p.Build(map[string]string{"slug": t.Slug, "path": t.Slug})
				if err := s.autoRedirect(ctx, tx, q, a, from, to); err != nil {
					return err
				}
				if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Paths: []string{from, to}}); err != nil {
					return err
				}
			}
		}
		if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: []jobs.Dep{{Type: "term", ID: id.String()}}}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "term.update", ResourceType: "term", ResourceID: id.String()})
	})
	return out, err
}

// DeleteTerm removes a term; with mergeInto set, entries are reassigned first.
func (s *Service) DeleteTerm(ctx context.Context, id uuid.UUID, mergeInto *uuid.UUID) error {
	a, err := actor.Require(ctx, rbac.TermsManage)
	if err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		old, err := q.GetTerm(ctx, id)
		if err != nil {
			return ErrTermNotFound
		}
		affected, err := q.EntriesWithTerm(ctx, id)
		if err != nil {
			return err
		}
		var paths []string
		if p := s.Site().Taxonomies[old.Taxonomy].Pattern(); p != nil {
			paths = append(paths, p.Build(map[string]string{"slug": old.Slug, "path": old.Slug}))
		}
		if mergeInto != nil {
			into, err := q.GetTerm(ctx, *mergeInto)
			if err != nil || into.Taxonomy != old.Taxonomy || into.ID == id {
				return fields.Errors{"merge_into": "must be another term in the same taxonomy"}
			}
			if err := q.ReassignTerm(ctx, dbq.ReassignTermParams{ToID: into.ID, FromID: id}); err != nil {
				return err
			}
			if len(paths) > 0 {
				p := s.Site().Taxonomies[old.Taxonomy].Pattern()
				if err := s.autoRedirect(ctx, tx, q, a, paths[0], p.Build(map[string]string{"slug": into.Slug, "path": into.Slug})); err != nil {
					return err
				}
			}
		}
		if err := q.DeleteTerm(ctx, id); err != nil {
			return err
		}
		deps := []jobs.Dep{{Type: "term", ID: id.String()}}
		for _, e := range affected {
			deps = append(deps, jobs.Dep{Type: "entry", ID: e.String()})
		}
		if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: deps, Paths: paths}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "term.delete", ResourceType: "term", ResourceID: id.String(),
			Diff: map[string]any{"taxonomy": old.Taxonomy, "slug": old.Slug, "entries": len(affected)}})
	})
}
