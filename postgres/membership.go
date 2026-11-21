package postgres

import (
	"encoding/json"
	"unicode/utf8"

	g "github.com/StuHelper/stuhelper-groups"
)

const ownerGroupsQuery = `SELECT c.id,c.data,m.data FROM groups.records m
 JOIN groups.records c ON c.bucket='config' AND c.id=left(m.bucket,length(m.bucket)-8)
 WHERE right(m.bucket,8)='/members' AND m.id=$1 AND m.data->>'owner'=$1
 AND m.data->>'state'<>'removed' AND ($4::boolean OR m.data->>'joined'='true')
 AND c.id COLLATE "C">$2 COLLATE "C" ORDER BY c.id COLLATE "C" LIMIT $3`

// OwnerGroups and the personal page share one query, index and cursor policy.
func (t *transaction) OwnerGroups(owner, cursor string, limit int, includeApplicants bool) (g.Page[g.ManagedGroup], error) {
	out := g.Page[g.ManagedGroup]{Items: []g.ManagedGroup{}}
	records, e := t.OwnerGroupRecords(owner, cursor, limit, includeApplicants)
	if e != nil {
		return out, e
	}
	for _, record := range records.Items {
		out.Items = append(out.Items, g.ManagedGroup{ID: record.Config.ID, School: record.Config.School})
	}
	out.NextCursor = records.NextCursor
	return out, nil
}
func (t *transaction) OwnerGroupRecords(owner, cursor string, limit int, includeApplicants bool) (g.Page[g.OwnerGroupRecord], error) {
	out := g.Page[g.OwnerGroupRecord]{Items: []g.OwnerGroupRecord{}}
	if limit < 1 || limit > 100 || utf8.RuneCountInString(cursor) > 128 {
		return out, g.ErrInvalid
	}
	rows, e := t.tx.Query(t.ctx, ownerGroupsQuery, owner, cursor, limit+1, includeApplicants)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var config, member []byte
		if e = rows.Scan(&id, &config, &member); e != nil {
			return out, e
		}
		if len(out.Items) == limit {
			out.NextCursor = out.Items[limit-1].Config.ID
			break
		}
		var item g.OwnerGroupRecord
		if e = json.Unmarshal(config, &item.Config); e != nil {
			return out, e
		}
		if e = json.Unmarshal(member, &item.Member); e != nil {
			return out, e
		}
		item.Config.ID = id
		out.Items = append(out.Items, item)
	}
	if e = rows.Err(); e != nil {
		return g.Page[g.OwnerGroupRecord]{Items: []g.OwnerGroupRecord{}}, e
	}
	return out, nil
}

const directoryGroupsQuery = `SELECT c.id,c.data,m.data FROM groups.records c
 LEFT JOIN groups.records m ON m.bucket=c.id||'/members' AND m.id=$1 AND m.data->>'owner'=$1 AND m.data->>'state'<>'removed'
 WHERE c.bucket='config' AND c.data->>'discoverable'='true'
 AND ($2='' OR c.data->>'school'=$2)
 AND ($3='' OR COALESCE(c.data->>'category','')=$3)
 AND c.id COLLATE "C">$4 COLLATE "C"
 ORDER BY c.id COLLATE "C" LIMIT $5`

func (t *transaction) DirectoryGroups(owner, school, category, cursor string, limit int) ([]g.DirectoryRecord, error) {
	if limit < 1 || limit > 100 || utf8.RuneCountInString(cursor) > 128 {
		return nil, g.ErrInvalid
	}
	rows, e := t.tx.Query(t.ctx, directoryGroupsQuery, owner, school, category, cursor, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []g.DirectoryRecord{}
	for rows.Next() {
		var id string
		var config, member []byte
		if e = rows.Scan(&id, &config, &member); e != nil {
			return nil, e
		}
		var item g.DirectoryRecord
		if e = json.Unmarshal(config, &item.Config); e != nil {
			return nil, e
		}
		item.Config.ID = id
		if len(member) > 0 {
			var m g.Member
			if e = json.Unmarshal(member, &m); e != nil {
				return nil, e
			}
			item.Member = &m
		}
		out = append(out, item)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	return out, nil
}
