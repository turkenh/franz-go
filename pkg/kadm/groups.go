package kadm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// DescribedGroupMember is the detail of an individual group member as returned
// by a describe groups response.
type DescribedGroupMember struct {
	MemberID   string  // MemberID is the Kafka assigned member ID of this group member.
	InstanceID *string // InstanceID is a potential user assigned instance ID of this group member (KIP-345).
	ClientID   string  // ClientID is the Kafka client given ClientID of this group member.
	ClientHost string  // ClientHost is the host this member is running on.

	Join     kmsg.GroupMemberMetadata   // Join is what this member sent in its join group request; what it wants to consume.
	Assigned kmsg.GroupMemberAssignment // Assigned is what this member was assigned to consume by the leader.
}

// AssignedPartitions returns the set of unique topics and partitions that are
// assigned across all members in this group.
func (d *DescribedGroup) AssignedPartitions() TopicsSet {
	var s TopicsSet
	for _, m := range d.Members {
		for _, t := range m.Assigned.Topics {
			s.Add(t.Topic, t.Partitions...)
		}
	}
	return s
}

// DescribedGroup contains data from a describe groups response for a single
// group.
type DescribedGroup struct {
	Group string // Group is the name of the described group.

	Coordinator BrokerDetail           // Coordinator is the coordinator broker for this group.
	State       string                 // State is the state this group is in (Empty, Dead, Stable, etc.).
	Protocol    string                 // Protocol is the partition assignor strategy this group is using.
	Members     []DescribedGroupMember // Members contains the members of this group sorted first by InstanceID, or if nil, by MemberID.

	Err error // Err is non-nil if the group could not be described.
}

// DescribedGroups contains data for multiple groups from a describe groups
// response.
type DescribedGroups map[string]DescribedGroup

// AssignedPartitions returns the set of unique topics and partitions that are
// assigned across all members in all groups. This is the all-group analogue to
// DescribedGroup.AssignedPartitions.
func (ds DescribedGroups) AssignedPartitions() TopicsSet {
	var s TopicsSet
	for _, g := range ds {
		for _, m := range g.Members {
			for _, t := range m.Assigned.Topics {
				s.Add(t.Topic, t.Partitions...)
			}
		}
	}
	return s
}

// Sorted returns all groups sorted by group name.
func (ds DescribedGroups) Sorted() []DescribedGroup {
	s := make([]DescribedGroup, 0, len(ds))
	for _, d := range ds {
		s = append(s, d)
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Group < s[j].Group })
	return s
}

// Topics returns a sorted list of all group names.
func (ds DescribedGroups) Names() []string {
	all := make([]string, 0, len(ds))
	for g := range ds {
		all = append(all, g)
	}
	sort.Strings(all)
	return all
}

// ListedGroup contains data from a list groups response for a single group.
type ListedGroup struct {
	Group string // Group is the name of this group.
	State string // State is the state this group is in (Empty, Dead, Stable, etc.; only if talking to Kafka 2.6+).
}

// ListedGroups contains information from a list groups response.
type ListedGroups map[string]ListedGroup

// Sorted returns all groups sorted by group name.
func (ls ListedGroups) Sorted() []ListedGroup {
	s := make([]ListedGroup, 0, len(ls))
	for _, l := range ls {
		s = append(s, l)
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Group < s[j].Group })
	return s
}

// Groups returns a sorted list of all group names.
func (ls ListedGroups) Groups() []string {
	all := make([]string, 0, len(ls))
	for g := range ls {
		all = append(all, g)
	}
	sort.Strings(all)
	return all
}

// ListGroups returns all groups in the cluster. If you are talking to Kafka
// 2.6+, filter states can be used to return groups only in the requested
// states. By default, this returns all groups. In almost all cases,
// DescribeGroups is more useful.
//
// This may return *ShardErrors.
func (cl *Client) ListGroups(ctx context.Context, filterStates ...string) (ListedGroups, error) {
	req := kmsg.NewPtrListGroupsRequest()
	req.StatesFilter = append(req.StatesFilter, filterStates...)
	shards := cl.cl.RequestSharded(ctx, req)
	list := make(ListedGroups)
	return list, shardErrEach(req, shards, func(kr kmsg.Response) error {
		resp := kr.(*kmsg.ListGroupsResponse)
		if err := maybeAuthErr(resp.ErrorCode); err != nil {
			return err
		}
		if err := kerr.ErrorForCode(resp.ErrorCode); err != nil {
			return err
		}
		for _, g := range resp.Groups {
			list[g.Group] = ListedGroup{
				Group: g.Group,
				State: g.GroupState,
			}
		}
		return nil
	})
}

// DescribeGroups describes either all groups specified, or all groups in the
// cluster if none are specified.
//
// This may return *ShardErrors.
//
// If no groups are specified and this method first lists groups, and list
// groups returns a *ShardErrors, this function describes all successfully
// listed groups and appends the list shard errors to any returned describe
// shard errors.
//
// If only one group is described, there will be at most one request issued,
// and there is no need to deeply inspect the error.
func (cl *Client) DescribeGroups(ctx context.Context, groups ...string) (DescribedGroups, error) {
	var seList *ShardErrors
	if len(groups) == 0 {
		listed, err := cl.ListGroups(ctx)
		switch {
		case err == nil:
		case errors.As(err, &seList):
		default:
			return nil, err
		}
		groups = listed.Groups()
		if len(groups) == 0 {
			return nil, err
		}
	}

	req := kmsg.NewPtrDescribeGroupsRequest()
	req.Groups = groups

	shards := cl.cl.RequestSharded(ctx, req)
	described := make(DescribedGroups)
	err := shardErrEachBroker(req, shards, func(b BrokerDetail, kr kmsg.Response) error {
		resp := kr.(*kmsg.DescribeGroupsResponse)
		for _, rg := range resp.Groups {
			if err := maybeAuthErr(rg.ErrorCode); err != nil {
				return err
			}
			g := DescribedGroup{
				Group:       rg.Group,
				Coordinator: b,
				State:       rg.State,
				Protocol:    rg.Protocol,
				Err:         kerr.ErrorForCode(rg.ErrorCode),
			}
			for _, rm := range rg.Members {
				gm := DescribedGroupMember{
					MemberID:   rm.MemberID,
					InstanceID: rm.InstanceID,
					ClientID:   rm.ClientID,
					ClientHost: rm.ClientHost,
				}
				gm.Join.ReadFrom(rm.ProtocolMetadata)
				gm.Assigned.ReadFrom(rm.MemberAssignment)
				g.Members = append(g.Members, gm)
			}
			sort.Slice(g.Members, func(i, j int) bool {
				if g.Members[i].InstanceID != nil {
					if g.Members[j].InstanceID == nil {
						return true
					}
					return *g.Members[i].InstanceID < *g.Members[j].InstanceID
				}
				if g.Members[j].InstanceID != nil {
					return false
				}
				return g.Members[i].MemberID < g.Members[j].MemberID
			})
			described[g.Group] = g
		}
		return nil
	})

	var seDesc *ShardErrors
	switch {
	case err == nil:
		return described, seList.into()
	case errors.As(err, &seDesc):
		if seList != nil {
			seDesc.Errs = append(seList.Errs, seDesc.Errs...)
		}
		return described, seDesc.into()
	default:
		return nil, err
	}
}

// DeleteGroupResponse contains the response for an individual deleted group.
type DeleteGroupResponse struct {
	Group string // Group is the group this response is for.
	Err   error  // Err is non-nil if the group failed to be deleted.
}

// DeleteGroups deletes all groups specified.
//
// The purpose of this request is to allow operators a way to delete groups
// after Kafka 1.1, which removed RetentionTimeMillis from offset commits. See
// KIP-229 for more details.
//
// This may return *ShardErrors. This does not return on authorization
// failures, instead, authorization failures are included in the responses.
func (cl *Client) DeleteGroups(ctx context.Context, groups ...string) ([]DeleteGroupResponse, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	req := kmsg.NewPtrDeleteGroupsRequest()
	req.Groups = append(req.Groups, groups...)
	shards := cl.cl.RequestSharded(ctx, req)

	var resps []DeleteGroupResponse
	return resps, shardErrEach(req, shards, func(kr kmsg.Response) error {
		resp := kr.(*kmsg.DeleteGroupsResponse)
		for _, g := range resp.Groups {
			resps = append(resps, DeleteGroupResponse{
				Group: g.Group,
				Err:   kerr.ErrorForCode(g.ErrorCode),
			})
		}
		return nil
	})
}

// OffsetResponse contains the response for an individual offset for offset
// methods.
type OffsetResponse struct {
	Offset
	Err error // Err is non-nil if the offset operation failed.
}

// OffsetResponses contains per-partition responses to offset methods.
type OffsetResponses map[string]map[int32]OffsetResponse

// Keep filters the responses to only keep the input offsets.
func (os OffsetResponses) Keep(o Offsets) {
	os.DeleteFunc(func(r OffsetResponse) bool {
		if len(o) == 0 {
			return true // keep nothing, delete
		}
		ot := o[r.Topic]
		if ot == nil {
			return true // topic missing, delete
		}
		_, ok := ot[r.Partition]
		return !ok // does not exist, delete
	})
}

// DeleteFunc deletes any offset for which fn returns true.
func (os OffsetResponses) DeleteFunc(fn func(OffsetResponse) bool) {
	for t, ps := range os {
		for p, o := range ps {
			if fn(o) {
				delete(ps, p)
			}
		}
		if len(ps) == 0 {
			delete(os, t)
		}
	}
}

// EachError calls fn for every offset that as a non-nil error.
func (os OffsetResponses) EachError(fn func(o OffsetResponse)) {
	for _, ps := range os {
		for _, o := range ps {
			if o.Err != nil {
				fn(o)
			}
		}
	}
}

// Each calls fn for every offset.
func (os OffsetResponses) Each(fn func(OffsetResponse)) {
	for _, ps := range os {
		for _, o := range ps {
			fn(o)
		}
	}
}

// Error iterates over all offsets and returns the first error encountered, if
// any. This can be used to check if an operation was entirely successful or
// not.
//
// Note that offset operations can be partially successful. For example, some
// offsets could succeed in an offset commit while others fail (maybe one topic
// does not exist for some reason, or you are not authorized for one topic). If
// This is something you need to worry about, you may need to check all offsets
// manually.
func (os OffsetResponses) Error() error {
	for _, ps := range os {
		for _, o := range ps {
			if o.Err != nil {
				return o.Err
			}
		}
	}
	return nil
}

// Ok returns true if there are no errors. This is a shortcut for os.Error() ==
// nil.
func (os OffsetResponses) Ok() bool {
	return os.Error() == nil
}

// CommitOffsets issues an offset commit request for the input offsets.
//
// This function can be used to manually commit offsets when directly consuming
// partitions outside of an actual consumer group. For example, if you assign
// partitions manually, but want still use Kafka to checkpoint what you have
// consumed, you can manually issue an offset commit request with this method.
//
// This does not return on authorization failures, instead, authorization
// failures are included in the responses.
func (cl *Client) CommitOffsets(ctx context.Context, group string, os Offsets) (OffsetResponses, error) {
	req := kmsg.NewPtrOffsetCommitRequest()
	req.Group = group
	for t, ps := range os {
		rt := kmsg.NewOffsetCommitRequestTopic()
		rt.Topic = t
		for p, o := range ps {
			rp := kmsg.NewOffsetCommitRequestTopicPartition()
			rp.Partition = p
			rp.Offset = o.Offset
			if len(o.Metadata) > 0 {
				rp.Metadata = kmsg.StringPtr(o.Metadata)
			}
			if o.CommitLeaderEpoch {
				rp.LeaderEpoch = o.LeaderEpoch
			}
		}
	}

	resp, err := req.RequestWith(ctx, cl.cl)
	if err != nil {
		return nil, err
	}

	rs := make(OffsetResponses)
	for i, t := range resp.Topics {
		rt := make(map[int32]OffsetResponse)
		rs[t.Topic] = rt
		if i >= len(req.Topics) {
			return nil, fmt.Errorf("topic %q at response index %d was not in offset commit request", t.Topic, i)
		}
		reqt := req.Topics[i]
		if reqt.Topic != t.Topic {
			return nil, fmt.Errorf("topic %q at response index %d does not match request topic %q", t.Topic, i, reqt.Topic)
		}
		for j, p := range t.Partitions {
			if j >= len(reqt.Partitions) {
				return nil, fmt.Errorf("topic %q partition %d at response index %d was not in offset commit request", t.Topic, p.Partition, j)
			}
			reqp := reqt.Partitions[j]
			if reqp.Partition != p.Partition {
				return nil, fmt.Errorf("topic %q partition %d at response index %d does not match request partition %d", t.Topic, p.Partition, j, reqp.Partition)
			}
			rt[p.Partition] = OffsetResponse{
				Offset: os[t.Topic][p.Partition],
				Err:    kerr.ErrorForCode(p.ErrorCode),
			}
		}
	}
	return rs, nil
}

// FetchOffsets issues an offset fetch requests for all topics and partitions
// in the group. Because Kafka returns only partitions you are authorized to
// fetch, this only returns an auth error if you are not authorized to describe
// the group at all.
//
// This method requires talking to Kafka v0.11+.
func (cl *Client) FetchOffsets(ctx context.Context, group string) (OffsetResponses, error) {
	req := kmsg.NewPtrOffsetFetchRequest()
	req.Group = group
	resp, err := req.RequestWith(ctx, cl.cl)
	if err != nil {
		return nil, err
	}
	if err := maybeAuthErr(resp.ErrorCode); err != nil {
		return nil, err
	}
	if err := kerr.ErrorForCode(resp.ErrorCode); err != nil {
		return nil, err
	}
	rs := make(OffsetResponses)
	for _, t := range resp.Topics {
		rt := make(map[int32]OffsetResponse)
		rs[t.Topic] = rt
		for _, p := range t.Partitions {
			if err := maybeAuthErr(p.ErrorCode); err != nil {
				return nil, err
			}
			var meta string
			if p.Metadata != nil {
				meta = *p.Metadata
			}
			rt[p.Partition] = OffsetResponse{
				Offset: Offset{
					Topic:       t.Topic,
					Partition:   p.Partition,
					Offset:      p.Offset,
					LeaderEpoch: p.LeaderEpoch,
					Metadata:    meta,
				},
				Err: kerr.ErrorForCode(p.ErrorCode),
			}
		}
	}
	return rs, nil
}

// FetchOffsetsResponse contains a fetch offsets response for a single group.
type FetchOffsetsResponse struct {
	Group   string          // Group is the offsets these fetches correspond to.
	Fetched OffsetResponses // Fetched contains offsets fetched for this group, if any.
	Err     error           // Err contains any error preventing offsets from being fetched.
}

// FetchOFfsetsResponses contains responses for many fetch offsets requests.
type FetchOffsetsResponses map[string]FetchOffsetsResponse

// EachError calls fn for every response that as a non-nil error.
func (rs FetchOffsetsResponses) EachError(fn func(FetchOffsetsResponse)) {
	for _, r := range rs {
		if r.Err != nil {
			fn(r)
		}
	}
}

// AllFailed returns whether all fetch offsets requests failed.
func (rs FetchOffsetsResponses) AllFailed() bool {
	var n int
	rs.EachError(func(FetchOffsetsResponse) { n++ })
	return n == len(rs)
}

// FetchManyOffsets issues a fetch offsets requests for each group specified.
//
// This API is slightly different from others on the admin client: the
// underlying FetchOFfsets request only supports one group at a time. Unlike
// all other methods, which build and issue a single request, this method
// issues many requests and captures all responses into the return map
// (disregarding sharded functions, which actually have the input request
// split).
//
// More importantly, FetchOffsets and CommitOffsets are important to provide as
// simple APIs for users that manage group offsets outside of a consumer group.
// This function complements FetchOffsets by supporting the metric gathering
// use case of fetching offsets for many groups at once.
func (cl *Client) FetchManyOffsets(ctx context.Context, groups ...string) FetchOffsetsResponses {
	if len(groups) == 0 {
		return nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	fetched := make(FetchOffsetsResponses)
	for i := range groups {
		group := groups[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			offsets, err := cl.FetchOffsets(ctx, group)
			mu.Lock()
			defer mu.Unlock()
			fetched[group] = FetchOffsetsResponse{
				Group:   group,
				Fetched: offsets,
				Err:     err,
			}
		}()
	}
	wg.Wait()
	return fetched
}

// DeleteOffsetsResponses contains the per topic, per partition errors. If an
// offset deletion for a partition was successful, the error will be nil.
type DeleteOffsetsResponses map[string]map[int32]error

// EachError calls fn for every partition that as a non-nil deletion error.
func (ds DeleteOffsetsResponses) EachError(fn func(string, int32, error)) {
	for t, ps := range ds {
		for p, err := range ps {
			if err != nil {
				fn(t, p, err)
			}
		}
	}
}

// DeleteOffsets deletes offsets for the given group.
//
// Originally, offset commits were persisted in Kafka for some retention time.
// This posed problematic for infrequently committing consumers, so the
// retention time concept was removed in Kafka v2.1 in favor of deleting
// offsets for a group only when the group became empty. However, if a group
// stops consuming from a topic, then the offsets will persist and lag
// monitoring for the group will notice an ever increasing amount of lag for
// these no-longer-consumed topics. Thus, Kafka v2.4 introduced an OffsetDelete
// request to allow admins to manually delete offsets for no longer consumed
// topics.
//
// This method requires talking to Kafka v2.4+. This returns an *AuthErr if the
// user is not authorized to delete offsets in the group at all. This does not
// return on per-topic authorization failures, instead, per-topic authorization
// failures are included in the responses.
func (cl *Client) DeleteOffsets(ctx context.Context, group string, s TopicsSet) (DeleteOffsetsResponses, error) {
	if len(s) == 0 {
		return nil, nil
	}

	req := kmsg.NewPtrOffsetDeleteRequest()
	req.Group = group
	for t, ps := range s {
		rt := kmsg.NewOffsetDeleteRequestTopic()
		rt.Topic = t
		for p := range ps {
			rp := kmsg.NewOffsetDeleteRequestTopicPartition()
			rp.Partition = p
			rt.Partitions = append(rt.Partitions, rp)
		}
		req.Topics = append(req.Topics, rt)
	}

	resp, err := req.RequestWith(ctx, cl.cl)
	if err != nil {
		return nil, err
	}
	if err := maybeAuthErr(resp.ErrorCode); err != nil {
		return nil, err
	}
	if err := kerr.ErrorForCode(resp.ErrorCode); err != nil {
		return nil, err
	}

	r := make(DeleteOffsetsResponses)
	for _, t := range resp.Topics {
		rt := make(map[int32]error)
		r[t.Topic] = rt
		for _, p := range t.Partitions {
			rt[p.Partition] = kerr.ErrorForCode(p.ErrorCode)
		}
	}
	return r, nil
}

// GroupMemberLag is the lag between a group member's current offset commit and
// the current end offset.
//
// If either the offset commits have load errors, or the listed end offsets
// have load errors, the Lag field will be -1 and the Err field will be set (to
// the first of either the commit error, or else the list error).
type GroupMemberLag struct {
	Member *DescribedGroupMember // Member is a reference to the group member consuming this partition.

	Commit Offset       // Commit is this member's current offset commit.
	End    ListedOffset // EndOffset is a reference to the end offset of this partition.
	Lag    int64        // Lag is how far behind this member is, or -1 if there is a commit error or list offset error.

	Err error // Err is either the commit error, or the list end offsets error, or nil.
}

// GroupLag is the per-topic, per-partition lag of members in a group.
type GroupLag map[string]map[int32]GroupMemberLag

// Sorted returns the per-topic, per-partition lag by member sorted in order by
// topic then partition.
func (l GroupLag) Sorted() []GroupMemberLag {
	var all []GroupMemberLag
	for _, ps := range l {
		for _, l := range ps {
			all = append(all, l)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		l, r := all[i], all[j]
		if l.End.Topic < r.End.Topic {
			return true
		}
		if l.End.Topic > r.End.Topic {
			return false
		}
		return l.End.Partition < r.End.Partition
	})
	return all
}

// CalculateGroupLag returns the per-partition lag of all members in a group.
// The input to this method is the returns from the three following methods,
//
//     DescribeGroups(ctx, group)
//     FetchOffsets(ctx, group)
//     ListEndOffsets(ctx, described.AssignedPartitions().Topics())
//
// If assigned partitions are missing in the listed end offsets listed end
// offsets, the partition will have an error indicating it is missing. A
// missing topic or partition in the commits is assumed to be nothing
// committing yet.
func CalculateGroupLag(
	group DescribedGroup,
	commit OffsetResponses,
	offsets ListedOffsets,
) GroupLag {
	l := make(map[string]map[int32]GroupMemberLag)

	for mi, m := range group.Members {
		for _, t := range m.Assigned.Topics {
			lt := l[t.Topic]
			if lt == nil {
				lt = make(map[int32]GroupMemberLag)
				l[t.Topic] = lt
			}

			tcommit := commit[t.Topic]
			tend := offsets[t.Topic]
			for _, p := range t.Partitions {
				var (
					pcommit OffsetResponse
					pend    ListedOffset
					perr    error
					ok      bool
				)

				if tcommit != nil {
					if pcommit, ok = tcommit[p]; !ok {
						pcommit = OffsetResponse{Offset: Offset{
							Topic:     t.Topic,
							Partition: p,
							Offset:    -1,
						}}
					}
				}
				if tend == nil {
					perr = errListMissing
				} else {
					if pend, ok = tend[p]; !ok {
						perr = errListMissing
					}
				}

				if perr == nil {
					if perr = pcommit.Err; perr == nil {
						perr = pend.Err
					}
				}

				lag := int64(-1)
				if perr == nil {
					lag = pend.Offset
					if pcommit.Offset.Offset >= 0 {
						lag = pend.Offset - pcommit.Offset.Offset
					}
				}

				lt[p] = GroupMemberLag{
					Member: &group.Members[mi],
					Commit: pcommit.Offset,
					End:    pend,
					Lag:    lag,
					Err:    perr,
				}

			}
		}
	}

	return l
}

var errListMissing = errors.New("missing from list offsets")
