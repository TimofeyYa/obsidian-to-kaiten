package syncengine

import (
	"sort"
	"strconv"
	"time"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/obsidian"
)

// Direction — направление действия для конкретного документа.
type Direction int

const (
	Unchanged Direction = iota
	RemoteNewer
	LocalNewer
	Conflict
	NewRemote
	NewLocal
	DeletedRemote
)

func (d Direction) String() string {
	switch d {
	case Unchanged:
		return "unchanged"
	case RemoteNewer:
		return "remote_newer"
	case LocalNewer:
		return "local_newer"
	case Conflict:
		return "conflict"
	case NewRemote:
		return "new_remote"
	case NewLocal:
		return "new_local"
	case DeletedRemote:
		return "deleted_remote"
	}
	return "unknown"
}

// Decision — что делать с конкретным документом.
type Decision struct {
	Direction Direction
	KaitenID  int
	Remote    *kaiten.Document // может быть nil для NewLocal/DeletedRemote
	Local     *obsidian.File   // может быть nil для NewRemote
	Prev      *DocState        // запись из state.json, может быть nil
}

// Tolerance — допустимое расхождение времени, чтобы не зацикливать запись.
const Tolerance = 2 * time.Second

// Decide определяет направление для одного документа.
func Decide(remote *kaiten.Document, local *obsidian.File, prev *DocState) Decision {
	d := Decision{Remote: remote, Local: local, Prev: prev}
	switch {
	case remote != nil:
		d.KaitenID = remote.ID
	case local != nil:
		d.KaitenID = local.Frontmatter.KaitenID
	}

	// 1. Только удалённый.
	if remote != nil && local == nil {
		d.Direction = NewRemote
		return d
	}
	// 2. Только локальный.
	if remote == nil && local != nil {
		if prev != nil {
			d.Direction = DeletedRemote
		} else {
			d.Direction = NewLocal
		}
		return d
	}
	// 3. Есть обе версии.
	remoteChanged := prev == nil || remote.Updated.After(prev.KaitenUpdated.Add(Tolerance))
	localChanged := prev == nil ||
		local.Mtime.After(prev.LocalMtime.Add(Tolerance)) ||
		local.ContentHash() != prev.ContentHash

	switch {
	case !remoteChanged && !localChanged:
		d.Direction = Unchanged
	case remoteChanged && !localChanged:
		d.Direction = RemoteNewer
	case !remoteChanged && localChanged:
		d.Direction = LocalNewer
	default:
		d.Direction = Conflict
	}
	return d
}

// BuildDecisions сводит remotes, locals и state в список решений.
// Результат отсортирован по KaitenID для детерминированности.
func BuildDecisions(remotes []kaiten.Document, locals []obsidian.File, st *State) []Decision {
	remoteByID := make(map[int]*kaiten.Document, len(remotes))
	for i := range remotes {
		remoteByID[remotes[i].ID] = &remotes[i]
	}
	localByID := make(map[int]*obsidian.File, len(locals))
	for i := range locals {
		localByID[locals[i].Frontmatter.KaitenID] = &locals[i]
	}

	// Собираем уникальные ID и сортируем — иначе порядок недетерминирован
	// (итерация по map в Go рандомизирована).
	idSet := make(map[int]struct{}, len(remoteByID)+len(localByID))
	for id := range remoteByID {
		idSet[id] = struct{}{}
	}
	for id := range localByID {
		idSet[id] = struct{}{}
	}
	ids := make([]int, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	decisions := make([]Decision, 0, len(ids))
	for _, id := range ids {
		r := remoteByID[id]
		l := localByID[id]
		prev := lookupState(st, id)
		decisions = append(decisions, Decide(r, l, prev))
	}
	return decisions
}

func lookupState(s *State, id int) *DocState {
	if s == nil {
		return nil
	}
	if v, ok := s.Documents[strconv.Itoa(id)]; ok {
		return &v
	}
	return nil
}
