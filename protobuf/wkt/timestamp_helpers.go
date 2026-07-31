package wkt

import "time"

// AsTime returns the Timestamp as a time.Time in UTC. Mirrors the
// google.golang.org/protobuf/types/known/timestamppb.Timestamp.AsTime helper.
func (t *Timestamp) AsTime() time.Time {
	return time.Unix(t.Seconds, int64(t.Nanos)).UTC()
}

// TimestampFromTime builds a Timestamp from a time.Time. Mirrors timestamppb.New.
func TimestampFromTime(t time.Time) *Timestamp {
	return &Timestamp{Seconds: t.Unix(), Nanos: int32(t.Nanosecond())}
}

// AsDuration returns the Duration as a time.Duration. Mirrors
// google.golang.org/protobuf/types/known/durationpb.Duration.AsDuration.
func (d *Duration) AsDuration() time.Duration {
	return time.Duration(d.Seconds)*time.Second + time.Duration(d.Nanos)
}

// DurationFromDuration builds a Duration from a time.Duration. Mirrors
// durationpb.New.
func DurationFromDuration(d time.Duration) *Duration {
	secs := int64(d / time.Second)
	nanos := int32(d % time.Second)
	return &Duration{Seconds: secs, Nanos: nanos}
}
