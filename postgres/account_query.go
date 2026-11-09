package postgres

import (
	"context"
	"encoding/json"
	aq "github.com/StuHelper/stuhelper-groups/accountquery"
	"sort"
)

// ReadAccount filters authoritative module facts before the stable exclusive page.
func (r *Repository) ReadAccount(ctx context.Context, s aq.Selection) ([]aq.Record, error) {
	scopes, _ := json.Marshal(s.Scopes)
	sensitive, _ := json.Marshal(s.SensitiveScopes)
	p, err := aq.Parse(s.Query.Value)
	if err != nil {
		return nil, aq.ErrInvalid
	}
	rows, err := r.Pool.Query(ctx, accountQuerySQL, s.Query.Value, s.Query.After, scopes, aq.Domain, s.Query.Limit, sensitive, p.Issuer, p.Subject)
	if err != nil {
		return nil, aq.ErrUnavailable
	}
	defer rows.Close()
	out := []aq.Record{}
	for rows.Next() {
		var v aq.Record
		var fields []byte
		var secret string
		if err = rows.Scan(&v.ID, &v.Scope.School, &v.Scope.Group, &v.ChangedAt, &v.Current, &fields, &secret); err != nil {
			return nil, aq.ErrUnavailable
		}
		v.Scope.Module = aq.Domain
		v.Subject = s.Query.Value
		v.Source = aq.Domain
		var values map[string]*string
		if json.Unmarshal(fields, &values) != nil {
			return nil, aq.ErrUnavailable
		}
		names := []string{}
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if values[name] != nil {
				v.Fields = append(v.Fields, aq.Field{Name: name, Category: "business", Value: *values[name]})
			}
		}

		out = append(out, v)
	}
	if rows.Err() != nil {
		return nil, aq.ErrUnavailable
	}
	return out, nil
}

const accountQuerySQL = `WITH records AS (SELECT 'groups:'||md5(r.bucket||':'||r.id) AS id,COALESCE(NULLIF(c.data->>'school',''),'*') AS school,c.id AS grp,
 r.query_changed_at AS changed,
 COALESCE(r.data->>'state','')<>'removed' AS current,
 jsonb_strip_nulls(jsonb_build_object('kind',split_part(r.bucket,'/',2),'state',r.data->>'state','joined',r.data->>'joined','deadline',r.data->>'deadline','exempt',r.data->>'exempt','extended',r.data->>'extended','count',r.data->>'count','until',r.data->>'until','permanent',r.data->>'permanent','action',COALESCE(r.data->'command'->>'kind',r.data->>'action'),'result',r.data->>'result')) AS fields,''::text AS sensitive
 FROM groups.records r JOIN groups.records c ON c.bucket='config' AND c.id=split_part(r.bucket,'/',1)
 WHERE ((split_part(r.bucket,'/',2) IN ('members','blacklist','warnings','watch') AND r.id=$1)
 OR (split_part(r.bucket,'/',2)='actions' AND r.data->'command'->>'target'=$1)
 OR (split_part(r.bucket,'/',2)='audit' AND (r.data->>'target'=$1 OR r.data->>'actor'=$1)) OR (split_part(r.bucket,'/',2) IN ('actions','audit') AND r.data->'actor'->>'issuer'=$7 AND r.data->'actor'->>'subject'=$8))) SELECT id,school,grp,changed,current,fields,sensitive FROM records
 WHERE id COLLATE "C">$2 COLLATE "C"
 AND EXISTS (SELECT 1 FROM jsonb_array_elements($3::jsonb) s WHERE (s->>'school'='*' OR s->>'school'=school) AND (s->>'group'='*' OR s->>'group'=grp))
 AND $4::text='groups' AND $1::text<>'' AND $6::jsonb IS NOT NULL
 ORDER BY id COLLATE "C" LIMIT $5`
