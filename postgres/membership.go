package postgres

import (
	g "github.com/StuHelper/stuhelper-groups"
)

const ownerGroupsQuery = `SELECT c.id,c.data->>'school' FROM groups.records m
 JOIN groups.records c ON c.bucket='config' AND c.id=left(m.bucket,length(m.bucket)-8)
 WHERE right(m.bucket,8)='/members' AND m.id=$1 AND m.data->>'owner'=$1
 AND m.data->>'state'<>'removed' AND ($4::boolean OR m.data->>'joined'='true')
 AND c.id COLLATE "C">$2 COLLATE "C" ORDER BY c.id COLLATE "C" LIMIT $3`

func (t *transaction) OwnerGroups(owner, cursor string, limit int, includeApplicants bool) (g.Page[g.ManagedGroup], error) {
	out := g.Page[g.ManagedGroup]{Items: []g.ManagedGroup{}}
	if limit < 1 || limit > 100 || len(cursor) > 128 {
		return out, g.ErrInvalid
	}
	rows, e := t.tx.Query(t.ctx, ownerGroupsQuery, owner, cursor, limit+1, includeApplicants)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var group g.ManagedGroup
		if e = rows.Scan(&group.ID, &group.School); e != nil {
			return out, e
		}
		if len(out.Items) == limit {
			out.NextCursor = out.Items[limit-1].ID
			break
		}
		out.Items = append(out.Items, group)
	}
	if e = rows.Err(); e != nil {
		return g.Page[g.ManagedGroup]{Items: []g.ManagedGroup{}}, e
	}
	return out, nil
}
