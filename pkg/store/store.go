package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/conprof/conprof/pkg/runutil"
	"github.com/conprof/conprof/pkg/store/storepb"
	"github.com/conprof/db/storage"
	"github.com/go-kit/kit/log"
	"github.com/prometheus/prometheus/pkg/labels"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type db interface {
	storage.Queryable
	storage.ChunkQueryable
	storage.Appendable
}

type profileStore struct {
	logger           log.Logger
	db               db
	maxBytesPerFrame int
}

func RegisterStoreServer(storeSrv storepb.ProfileStoreServer) func(*grpc.Server) {
	return func(s *grpc.Server) {
		storepb.RegisterProfileStoreServer(s, storeSrv)
	}
}

func NewProfileStore(logger log.Logger, db db, maxBytesPerFrame int) *profileStore {
	return &profileStore{
		logger:           logger,
		db:               db,
		maxBytesPerFrame: maxBytesPerFrame,
	}
}

var _ storepb.ProfileStoreServer = &profileStore{}

func (s *profileStore) Write(ctx context.Context, r *storepb.WriteRequest) (*storepb.WriteResponse, error) {
	app := s.db.Appender(ctx)
	for _, series := range r.ProfileSeries {
		ls := make([]labels.Label, 0, len(series.Labels))
		for _, l := range series.Labels {
			ls = append(ls, labels.Label{
				Name:  l.Name,
				Value: l.Value,
			})
		}

		for _, sample := range series.Samples {
			_, err := app.Add(ls, sample.Timestamp, sample.Value)
			if err != nil {
				return nil, err
			}
		}
	}

	return nil, app.Commit()
}

func (s *profileStore) Profile(ctx context.Context, r *storepb.ProfileRequest) (*storepb.ProfileResponse, error) {
	q, err := s.db.Querier(ctx, r.Timestamp, r.Timestamp)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	m, err := translatePbMatchers(r.Matchers)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "could not translate matchers: %v", err)
	}

	ss := q.Select(false, nil, m...)
	ok := ss.Next()
	if !ok {
		return nil, status.Error(codes.NotFound, "profile series not found")
	}

	i := ss.At().Iterator()
	ok = i.Seek(r.Timestamp)
	if !ok {
		return nil, errors.New("profile not found")
	}

	_, buf := i.At()
	return &storepb.ProfileResponse{
		Data: buf,
	}, nil
}

func (s *profileStore) Series(r *storepb.SeriesRequest, srv storepb.ProfileStore_SeriesServer) error {
	m, err := translatePbMatchers(r.Matchers)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "could not translate matchers: %v", err)
	}

	q, err := s.db.ChunkQuerier(srv.Context(), r.MinTime, r.MaxTime)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	defer runutil.CloseWithLogOnErr(s.logger, q, "close tsdb chunk querier series")

	set := q.Select(false, nil, m...)
	for set.Next() {
		series := set.At()
		labels := translatePromLabels(series.Labels())
		bytesLeftForChunks := s.maxBytesPerFrame
		for _, lbl := range labels {
			bytesLeftForChunks -= lbl.Size()
		}
		frameBytesLeft := bytesLeftForChunks

		seriesChunks := []storepb.Chunk{}

		chIter := series.Iterator()
		isNext := chIter.Next()
		for isNext {
			chk := chIter.At()
			if chk.Chunk == nil {
				return status.Errorf(codes.Internal, "TSDBStore: found not populated chunk returned by SeriesSet at ref: %v", chk.Ref)
			}

			c := storepb.Chunk{
				MinTime: chk.MinTime,
				MaxTime: chk.MaxTime,
				Type:    storepb.Chunk_Encoding(chk.Chunk.Encoding() - 1), // Proto chunk encoding is one off to TSDB one.
				Data:    chk.Chunk.Bytes(),
			}
			frameBytesLeft -= c.Size()
			seriesChunks = append(seriesChunks, c)

			// We are fine with minor inaccuracy of max bytes per frame. The inaccuracy will be max of full chunk size.
			isNext = chIter.Next()
			if frameBytesLeft > 0 && isNext {
				continue
			}
			if err := srv.Send(storepb.NewSeriesResponse(&storepb.RawProfileSeries{Labels: labels, Chunks: seriesChunks})); err != nil {
				return status.Error(codes.Aborted, err.Error())
			}

			if isNext {
				frameBytesLeft = bytesLeftForChunks
				seriesChunks = make([]storepb.Chunk, 0, len(seriesChunks))
			}
		}
		if err := chIter.Err(); err != nil {
			return status.Error(codes.Internal, fmt.Errorf("chunk iter: %w", err).Error())
		}
	}

	if err := set.Err(); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	for _, w := range set.Warnings() {
		if err := srv.Send(storepb.NewWarnSeriesResponse(w)); err != nil {
			return status.Error(codes.Aborted, err.Error())
		}
	}
	return nil
}

func translatePromLabels(lbls []labels.Label) []storepb.Label {
	res := make([]storepb.Label, 0, len(lbls))
	for _, l := range lbls {
		res = append(res, storepb.Label{
			Name:  l.Name,
			Value: l.Value,
		})
	}

	return res
}

func translatePbLabels(lbls []storepb.Label) []labels.Label {
	res := make([]labels.Label, 0, len(lbls))
	for _, l := range lbls {
		res = append(res, labels.Label{
			Name:  l.Name,
			Value: l.Value,
		})
	}

	return res
}

func translatePbMatchers(ms []storepb.LabelMatcher) (res []*labels.Matcher, err error) {
	for _, m := range ms {
		r, err := translatePbMatcher(m)
		if err != nil {
			return nil, err
		}
		res = append(res, r)
	}
	return res, nil
}

func translatePbMatcher(m storepb.LabelMatcher) (*labels.Matcher, error) {
	switch m.Type {
	case storepb.LabelMatcher_EQ:
		return labels.NewMatcher(labels.MatchEqual, m.Name, m.Value)

	case storepb.LabelMatcher_NEQ:
		return labels.NewMatcher(labels.MatchNotEqual, m.Name, m.Value)

	case storepb.LabelMatcher_RE:
		return labels.NewMatcher(labels.MatchRegexp, m.Name, m.Value)

	case storepb.LabelMatcher_NRE:
		return labels.NewMatcher(labels.MatchNotRegexp, m.Name, m.Value)
	}
	return nil, fmt.Errorf("unknown label matcher type %d", m.Type)
}

func translatePromMatchers(ms []*labels.Matcher) (res []storepb.LabelMatcher, err error) {
	for _, m := range ms {
		r, err := translatePromMatcher(m)
		if err != nil {
			return nil, err
		}
		res = append(res, r)
	}
	return res, nil
}

func translatePromMatcher(m *labels.Matcher) (storepb.LabelMatcher, error) {
	switch m.Type {
	case labels.MatchEqual:
		return storepb.LabelMatcher{
			Type:  storepb.LabelMatcher_EQ,
			Name:  m.Name,
			Value: m.Value,
		}, nil

	case labels.MatchNotEqual:
		return storepb.LabelMatcher{
			Type:  storepb.LabelMatcher_NEQ,
			Name:  m.Name,
			Value: m.Value,
		}, nil

	case labels.MatchRegexp:
		return storepb.LabelMatcher{
			Type:  storepb.LabelMatcher_RE,
			Name:  m.Name,
			Value: m.Value,
		}, nil

	case labels.MatchNotRegexp:
		return storepb.LabelMatcher{
			Type:  storepb.LabelMatcher_NRE,
			Name:  m.Name,
			Value: m.Value,
		}, nil
	}
	return storepb.LabelMatcher{}, fmt.Errorf("unknown label matcher type %d", m.Type)
}
