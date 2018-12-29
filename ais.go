// Package ais provides types and methods for conducting data science
// on signals generated by maritime entities radiating from an Automated
// Identification System (AIS) transponder as mandated by the International
// Maritime Organization (IMO) for all vessels over 300 gross tons and all
// passenger vessels.
package ais

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/FATHOM5/haversine"
	"github.com/mmcloughlin/geohash"
)

// TimeLayout is the timestamp format for the MarineCadastre.gov
// AIS data available from the U.S. Government.  An example
// timestamp from the data set is `2017-12-05T00:01:14`.  This layout is
// designed to be passed to the time.Parse function as the layout
// string.
const TimeLayout = `2006-01-02T15:04:05`

// Unexported flushThreshhold is the number of records that a csv.Writer
// will write to memory before being flushed.
const flushThreshold = 250000

// ErrEmptySet is the error returned by Subset variants and RecordSet.Track
// when there are no records in the returned *RecordSet because nothing
// matched the selection criteria. Functions should only return ErrEmptySet when
// all processing occurred successfully, but the subset criteria provided no
// matches to return.
var ErrEmptySet = errors.New("ErrEmptySet")

// Beginning provides a time prior to the discovery of the transistor
// as a convenience time to enter as the `start` time in functions like
// RecordSet.Track.  It is highly unlikely that any ais RecordSet will include
// data before this time and therefore start==Beginning will ensure that all
// records from the beginning of the RecordSet are examined for matches.
var Beginning = time.Date(1940, 1, 1, 1, 0, 0, 0, time.UTC)

// All provides a duration of 200 years as a convenience argument for dur in
// RecordSet.Track(start time.Time, dur time.Duration).  The intent is that the
// start time plus All will encompass all records in the set.
var All = time.Hour * 24 * 365 * 200 // 200 years

// Matching provides an interface to pass into the Subset and LimitSubset functions
// of a RecordSet.
type Matching interface {
	Match(*Record) (bool, error)
}

// Definition is the struct format that JSON dictionary files are
// loaded into with ais.RecordSet.SetDictionary(blob []byte).
type Definition struct {
	Fieldname   string
	Description string
}

// Match is the function signature for the argument to ais.Matching
// used to match Records.  The variadic argument indices indicate the
// index numbers in the record for the fields that will be compared.
// type Match func(rec *Record) bool

// Vessel is a struct for the identifying information about a specific
// ship in an AIS dataset.  NOTE: REFINEMENT OF PACKAGE AIS WILL
// INCORPORATE MORE OF THE SHIP IDENTIFYING DATA COMMENTED OUT IN THIS
// MINIMALLY VIABLE IMPLEMENTATION.
type Vessel struct {
	MMSI       string
	VesselName string
	// IMO        string
	// Callsign   string
	// VesselType string
	// Length     string
	// Width      string
	// Draft      string
}

// VesselSet is a map[Type]bool set of unique vessels usually obtained
// by the return value of rs.UniqueVessels()
type VesselSet map[Vessel]bool

// Field is an abstraction for string values that are read from and
// written to AIS Records.
type Field string

// Generator is the interface that is implemented to create a new Field
// from the index values of existing Fields in a Record.  The receiver for
// Generator should be a pointer in order to avoid creating a copy of the
// Record when Generate is called millions of times iterating over a large
// RecordSet.  The Generator interface is used in the AppendField method
// of RecordSet.
type Generator interface {
	Generate(rec Record, index ...int) (Field, error)
}

// Geohasher is the base type for implementing the Generator interface and
// appends a github.commccloughlin/geohash to each Record in the RecordSet.
type Geohasher RecordSet

// NewGeohasher returns a pointer to a new Geohasher.  The simplest use of the
// GeoHasher type to append a geohash field to a RecordSet is to call AppendField
// and pass NewGeohasher(rs) as the Generator argument in AppendField.
func NewGeohasher(rs *RecordSet) *Geohasher {
	g := Geohasher(*rs)
	return &g
}

// Generate returns a geohash Field.  The geohash returned is accurate to 22
// bits of precision which corresponds to about .1 degree differences in
// lattitude and longitude.  The index values for the variadic function on
// a *Geohasher must be the index of "LAT" and "LON" in the Record, rec.  Field
// will come back nil for any non-nil error returned.
func (g *Geohasher) Generate(rec Record, index ...int) (Field, error) {
	if len(index) != 2 {
		return "", fmt.Errorf("geohash: generate: len(index) must equal" +
			" 2 where the first int is the index of `LAT` and the second int is the index of `LON`")
	}
	indexLat, indexLon := index[0], index[1]

	// From these values create a geohash and return it
	lat, err := rec.ParseFloat(indexLat)
	if err != nil {
		return "", fmt.Errorf("geohash: unable to parse lat")
	}
	lon, err := rec.ParseFloat(indexLon)
	if err != nil {
		return "", fmt.Errorf("geohash: unable to parse lon")
	}
	hash := geohash.EncodeIntWithPrecision(lat, lon, uint(22))
	return Field(fmt.Sprintf("%#x", hash)), nil
}

// RecordSet is an the high level interface to deal with comma
// separated value files of AIS records.  Unexported fields are
// encapsulated by getter methods.
type RecordSet struct {
	r     *csv.Reader   // internally held csv pointer
	w     *csv.Writer   // internally held csv pointer
	h     Headers       // Headers used to parse each Record
	data  io.ReadWriter // client provided io interface
	first *Record       // accessible only by package functions
	stash *Record       // stashed Record from a client Read() but not yet used
}

// NewRecordSet returns a *Recordset that has an in-memory data buffer for
// the underlying Records that may be written to it.  Additionally, the new
// *Recordset is configured so that the encoding/csv objects it uses internally
// has LazyQuotes = true and and Comment = '#'.
func NewRecordSet() *RecordSet {
	rs := new(RecordSet)

	buf := bytes.Buffer{}
	rs.data = &buf
	rs.r = csv.NewReader(&buf)
	rs.w = csv.NewWriter(&buf)

	rs.r.LazyQuotes = true
	rs.r.Comment = '#'

	return rs
}

// OpenRecordSet takes the filename of an ais data file as its input.
// It returns a pointer to the RecordSet and nil upon successfully validating
// that the file can be read by an encoding/csv Reader and that the first
// line of the file contains the minimum set of headers to create an
// ais.Report. It returns a nil Recordset and a non-nil error if the file
// cannot be opened or the headers do not pass validation.
func OpenRecordSet(filename string) (*RecordSet, error) {
	rs := NewRecordSet()

	f, err := os.OpenFile(filename, os.O_RDWR, 0666) // 0666 - Read Write
	if err != nil {
		return nil, fmt.Errorf("open recordset: %v", err)
	}
	rs.data = f
	rs.r = csv.NewReader(f)
	rs.r.LazyQuotes = true
	rs.r.Comment = '#'

	rs.w = csv.NewWriter(f)

	// The first line of a valid ais datafile should contain the headers.
	// The following Read() command also advances the file pointer so that
	// it now points at the first data line.
	var h Headers
	h.fields, err = rs.r.Read()
	if err != nil {
		return nil, fmt.Errorf("open recordset: %v", err)
	}
	rs.h = h

	return rs, nil
}

// SetHeaders provides the expected interface to a RecordSet
func (rs *RecordSet) SetHeaders(h Headers) {
	rs.h = h
}

// Read calls Read() on the csv.Reader held by the RecordSet and returns a
// Record.  The idiomatic way to iterate over a recordset comes from the
// same idiom to read a file using encoding/csv.
func (rs *RecordSet) Read() (*Record, error) {
	// When Read is called by clients they want the first Record. If that
	// Record has already been read by internal packages return the one that
	// was already read internally.
	if rec := rs.first; rec != nil {
		rs.first = nil
		return rec, nil
	}

	// Clients may Read() a Record but not use it and want to get that same
	// Record back on the next call to Read().  Stash allows this functionality
	// to work.
	if rec := rs.stash; rec != nil {
		rs.stash = nil
		return rec, nil
	}

	r, err := rs.r.Read()
	if err == io.EOF {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("recordset read: %v", err)
	}
	rec := Record(r)
	return &rec, nil
}

// ReadFirst is an unexported method used by various internal packages
// to get the first line of the RecordSet.
func (rs *RecordSet) readFirst() (*Record, error) {
	if rs.first != nil {
		return rs.first, nil
	}
	r, err := rs.r.Read()
	if err != nil {
		return nil, err
	}
	rec := Record(r)
	rs.first = &rec
	return rs.first, nil
}

// Stash allows a client to take Record that has been previously retrieved
// through Read() and ensure the next call to Read() returns this same
// Record.
func (rs *RecordSet) Stash(rec *Record) {
	rs.stash = rec
}

// Write calls Write() on the csv.Writer held by the RecordSet and returns an
// error.  The error is nil on a successful write.  Flush() should be called at
// the end of necessary Write() calls to ensure the IO buffer flushed.
func (rs *RecordSet) Write(rec Record) error {
	err := rs.w.Write(rec)
	return err
}

// Flush empties the buffer in the underlying csv.Writer held by the RecordSet
// and returns any error that has occurred in a previous write or flush.
func (rs *RecordSet) Flush() error {
	rs.w.Flush()
	err := rs.w.Error()
	return err
}

// AppendField calls the Generator on each Record in the RecordSet and adds
// the resulting Field to each record under the newField provided as the
// argument.  The requiredHeaders argument is a []string of the required Headers
// that must be present in the RecordSet in order for Generator to be successful.
// If no errors are encournterd it returns a pointer to a new *RecordSet and a
// nil value for error.  If there is an error it will return a nil value for
// the *RecordSet and an error.
func (rs *RecordSet) AppendField(newField string, requiredHeaders []string, gen Generator) (*RecordSet, error) {
	rs2 := NewRecordSet()

	h := rs.Headers()
	h.fields = append(h.fields, newField)
	rs2.SetHeaders(h)

	// Find the index values for the Generator
	var indices []int
	for _, target := range requiredHeaders {
		index, ok := rs.Headers().Contains(target)
		if !ok {
			return nil, fmt.Errorf("append: headers does not contain %s", target)
		}
		indices = append(indices, index)
	}

	// Iterate over the records
	written := 0
	for {
		var rec Record
		rec, err := rs.r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("append: read error on csv file: %v", err)
		}

		field, err := gen.Generate(rec, indices...)
		if err != nil {
			return nil, fmt.Errorf("appendfield: generate: %v", err)
		}
		rec = append(rec, string(field))
		err = rs2.Write(rec)
		if err != nil {
			return nil, fmt.Errorf("appendfield: csv write error: %v", err)
		}
		written++
		if written%flushThreshold == 0 {
			err := rs2.Flush()
			if err != nil {
				return nil, fmt.Errorf("appendfield: csv flush error: %v", err)
			}
		}

	}
	err := rs2.Flush()
	if err != nil {
		return nil, fmt.Errorf("appendfield: csv flush error: %v", err)
	}
	return rs2, nil
}

// Close calls close on the unexported RecordSet data handle.
// It is the responsibility of the RecordSet user to
// call close.  This is usually accomplished by a call to
//      defer rs.Close()
// immediately after creating a NewRecordSet.
func (rs *RecordSet) Close() error {
	if rs.data == nil {
		return nil
	}
	// Use reflection to determine if the underlying io.ReadWriter
	// can call Close()
	closerType := reflect.TypeOf((*io.Closer)(nil)).Elem()

	if reflect.TypeOf(rs.data).Implements(closerType) {
		v := reflect.ValueOf(rs.data).MethodByName("Close").Call(nil)
		if v[0].Interface() == (error)(nil) {
			return nil
		}
		err := v[0].Interface().(error)
		return fmt.Errorf("recordset close: %v", err)
	}

	// no-op for types that do not implement close
	return nil
}

// Headers returns the encapsulated headers data of the Recordset
func (rs *RecordSet) Headers() Headers { return rs.h }

// SetDictionary requires a JSON blob encoded as a []byte and tests the
// contents of this JSON to ensure that json.Unmarshal can read the data
// into a []ais.Definition. It return nil for the error value when the dictionary
// properly unmarshals and a dictionary is attached. Returns a non-nil error
// when the dictionary does not unmarshal into ais.Definition structs or the
// JSON fields do not match the field names for an ais.Definition.
func (rs *RecordSet) SetDictionary(blob []byte) error {

	// Decode the JSON blob
	var defs []Definition
	err := json.Unmarshal(blob, &defs)
	if err != nil {
		rs.h.dictionary = nil
		return fmt.Errorf("set dictionary: unmarshal blob: %v", err)
	}

	// No definition should have zero length fieldname, but when the blob
	// contains the wrong fieldnames it may unmarshal but will have zero length
	// values
	for _, def := range defs {
		if (len(def.Fieldname) == 0) || (len(def.Description) == 0) {
			return errors.New("set dictionary: fieldnames in json blob may not match ais.Definition struct fields")
		}
	}

	// create a map of the fields and descriptions in the recordset
	defMap := make(map[string]string)
	for _, def := range defs {
		defMap[strings.TrimSpace(def.Fieldname)] = strings.TrimSpace(def.Description)
	}

	rs.h.dictionary = defMap
	return nil
}

// SubsetLimit returns a pointer to a new RecordSet with the first n records that
// return true from calls to Match(*Record) (bool, error) on the provided argument m
// that implements the Matching interface.
// Returns nil for the *RecordSet when error is non-nil.
// For n values less than zero, SubsetLimit will return all matches in the set.
func (rs *RecordSet) SubsetLimit(m Matching, n int) (*RecordSet, error) {
	rs2 := NewRecordSet()
	rs2.SetHeaders(rs.Headers())

	// In order to reset the read pointer of rs to the same data it was pointing at
	// when entering the function we create a new buffer and write all Reads to it.
	// Then point rs.r to this buffer at the end of the function. A more (MUCH MORE)
	// efficient solution would probably be controlling the Seek() value of the underlying
	// decriptor, but csv.Reader does not expose this pointer.
	copyBuf := &bytes.Buffer{}
	copyWriter := bufio.NewWriter(copyBuf)

	recordsLeftToWrite := n
	for recordsLeftToWrite != 0 {
		var rec *Record
		rec, err := rs.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("subset: read error on csv file: %v", err)
		}
		copyWriter.Write(rec.Data())

		match, err := m.Match(rec)
		if err != nil {
			return nil, err
		}

		if match {
			err := rs2.Write(*rec)
			if err != nil {
				return nil, fmt.Errorf("subset: csv write error: %v", err)
			}
			recordsLeftToWrite--
			if recordsLeftToWrite%flushThreshold == 0 {
				err := rs2.Flush()
				if err != nil {
					return nil, fmt.Errorf("subset: csv flush error: %v", err)
				}
			}
		}
	}
	err := rs2.Flush()
	if err != nil {
		return nil, fmt.Errorf("subset: csv flush error: %v", err)
	}

	if recordsLeftToWrite == n { // no change, therefore no records written
		return rs2, ErrEmptySet
	}

	copyWriter.Flush()
	rs.r = csv.NewReader(copyBuf)
	return rs2, nil
}

// Subset returns a pointer to a new *RecordSet that contains all of the records that
// return true from calls to Match(*Record) (bool, error) on the provided argument m
// that implements the Matching interface.
// Returns nil for the *RecordSet when error is non-nil.
func (rs *RecordSet) Subset(m Matching) (*RecordSet, error) {
	return rs.SubsetLimit(m, -1)
}

// UniqueVessels returns a VesselMap of every vessel in the dataset
// NOTE: THE VESSELMAP RETURNED IS BUILT ONLY ON THE MMSI AND VESSEL NAME
// OF THE SHIP.  OTHER FIELDS OF VESSEL MAY NEED TO BE ADDED FOR FUTURE
// ANALYSIS CAPABILITIES OF THE PACKAGE.
func (rs *RecordSet) UniqueVessels() (VesselSet, error) {
	vs := make(VesselSet)
	var defaultVesselName = "no VesselName header"

	mmsiIndex, ok := rs.Headers().Contains("MMSI")
	if !ok {
		return nil, fmt.Errorf("unique vessels: recordset does not contain MMSI header")
	}
	vesselNameIndex, okVesselName := rs.Headers().Contains("VesselName")

	var rec *Record
	var err error

	// In order to reset the read pointer of rs to the same data it was pointing at
	// when entering the function we create a new buffer and write all Reads to it.
	// Then point rs.r to this buffer at the end of the function. A more (MUCH MORE)
	// efficient solution would probably be controlling the Seek() value of the underlying
	// decriptor, but csv.Reader does not expose this pointer.
	copyBuf := &bytes.Buffer{}
	copyWriter := bufio.NewWriter(copyBuf)

	for {
		rec, err = rs.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("unique vessel: read error on csv file: %v", err)
		}
		copyWriter.Write(rec.Data())

		if okVesselName {
			vs[Vessel{MMSI: (*rec)[mmsiIndex], VesselName: (*rec)[vesselNameIndex]}] = true
		} else {
			vs[Vessel{MMSI: (*rec)[mmsiIndex], VesselName: defaultVesselName}] = true
		}
	}

	copyWriter.Flush()
	rs.r = csv.NewReader(copyBuf)
	return vs, nil
}

// SubsetByTrack is used within the Track function perform create a concrete Subset
type subsetByTrack struct {
	rs             *RecordSet
	m              int64
	s              time.Time
	d              time.Duration
	mmsiIndex      int
	timestampIndex int
}

// NewSubsetByTrack creates a new sbt structure and checks to ensure that the required
// headers are present before construction.
func newSubsetByTrack(rs *RecordSet, mmsi int64, start time.Time, dur time.Duration) (*subsetByTrack, error) {
	sbt := subsetByTrack{
		rs: rs,
		m:  mmsi,
		s:  start,
		d:  dur,
	}

	var ok bool
	sbt.mmsiIndex, ok = sbt.rs.Headers().Contains("MMSI")
	if !ok {
		return nil, fmt.Errorf("subsetByTrack: missing header MMSI")
	}
	sbt.timestampIndex, ok = sbt.rs.Headers().Contains("BaseDateTime")
	if !ok {
		return nil, fmt.Errorf("subsetByTrack: missing header BaseDateTime")
	}

	return &sbt, nil
}

// Match implements the Matching interface for subsetByType
func (sbt subsetByTrack) Match(rec *Record) (bool, error) {
	mmsi, err := rec.ParseInt(sbt.mmsiIndex)
	if err != nil {
		return false, fmt.Errorf("subsetByTrack: %v", err)
	}
	t, err := rec.ParseTime(sbt.timestampIndex)
	if err != nil {
		return false, fmt.Errorf("subsetByTrack: %v", err)
	}

	mmsiMatch := mmsi == sbt.m
	tAfterStart := t.After(sbt.s)
	future := sbt.s.Add(sbt.d)
	tBeforeEnd := t.Before(future)
	return mmsiMatch && tAfterStart && tBeforeEnd, nil
}

// Track returns a *RecordSet that contains a collection of ais.Record that are
// sequential in time and belong to the same MMSI.  Arguments to the function are the
// MMSI of the desired vessel, the start time to begin building the Track and the
// duration for the amount of time the Track should cover. The returned set contains
// records with a BaseDateTime on the open interval (start, start+dur). For any error
// the function returns nil for the returned RecordSet.
//
// Convenience variables ais.Beginning of type time.Time and ais.All of type time.Duration
// are provided in the package to use as the value of start and dur in order to start at
// the beginning of a RecordSet and return all matches.
//
// In addition to normal errors returned when the function cannot successfully execute,
// the returned error also includes a semephore built in the same implementation as io.EOF
// so that clients of the Track function can test for an empty RecordSet.  This error
// returns true from the comparison err == ais.ErrEmptyTrack.
func (rs *RecordSet) Track(mmsi int64, start time.Time, dur time.Duration) (*RecordSet, error) {
	sbt, err := newSubsetByTrack(rs, mmsi, start, dur)
	if err != nil {
		return nil, err
	}

	rs2, err := rs.Subset(sbt)
	if err == ErrEmptySet {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("track: %v", err)
	}

	return rs2, nil
}

// Box provides a type with min and max values for latitude and longitude, and Box
// implements the Matching interface.  This provides a convenient way to create a
// Box and pass the new object to Subset in order to get a *RecordSet defined
// with a geographic boundary.  Box includes records that are on the border
// and at the vertices of the geographic boundary. Constructing a box also requires
// the index value for lattitude and longitude in a *Record.  These index values will be
// called in *Record.ParseFloat(index) from the Match method of a Box in order to
// see if the Record is in the Box.
type Box struct {
	MinLat, Maxlat, MinLon, MaxLon float64
	LatIndex, LonIndex             int
}

// Match implements the Matching interface for a Box.  Errors in the Match function
// can be caused by parse errors when converting string Record values into their
// typed values. When Match returns a non-nil error the bool value will be false.
func (b *Box) Match(rec *Record) (bool, error) {
	lat, err := rec.ParseFloat(b.LatIndex)
	if err != nil {
		return false, fmt.Errorf("unable to parse %v", (*rec)[b.LatIndex])
	}
	lon, err := rec.ParseFloat(b.LonIndex)
	if err != nil {
		return false, fmt.Errorf("unable to parse %v", (*rec)[b.LonIndex])
	}

	return lat >= b.MinLat && lat <= b.Maxlat && lon >= b.MinLon && lon <= b.MaxLon, nil
}

// Save writes the RecordSet to disk in the filename provided
func (rs *RecordSet) Save(name string) error {
	var err error
	rs.data, err = os.Create(name)
	if err != nil {
		return fmt.Errorf("recordset save: %v", err)
	}
	rs.w = csv.NewWriter(rs.data) // FYI - csv uses bufio.NewWriter internally
	rs.Write(rs.h.fields)

	for {
		rec, err := rs.r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("recordset save: read error on csv file: %v", err)
		}
		rs.Write(rec)
	}
	err = rs.Flush()
	if err != nil {
		return fmt.Errorf("recordset save: flush error: %v", err)
	}

	return nil
}

// ByTimestamp implements the sort.Interface for creating a RecordSet
// sorted by BaseDateTime.
type ByTimestamp struct {
	h    Headers
	data *[]Record
}

// NewByTimestamp returns a data structure suitable for sorting using
// the sort.Interface tools.
func NewByTimestamp(rs *RecordSet) (*ByTimestamp, error) {
	bt := new(ByTimestamp)
	bt.h = rs.Headers()

	// Read the data from the underlying Recordset into a slice
	var err error
	bt.data, err = rs.loadRecords()
	if err != nil {
		return nil, fmt.Errorf("new bytimestamp: unable to load data: %v", err)
	}

	return bt, nil
}

// Len function to implement the sort.Interface.
func (bt ByTimestamp) Len() int { return len(*bt.data) }

// Swap function to implement the sort.Interface.
func (bt ByTimestamp) Swap(i, j int) {
	(*bt.data)[i], (*bt.data)[j] = (*bt.data)[j], (*bt.data)[i]
}

//Less function to implement the sort.Interface.
func (bt ByTimestamp) Less(i, j int) bool {
	timeIndex, ok := bt.h.Contains("BaseDateTime")
	if !ok {
		panic("bytimestamp: less: headers does not contain BaseDateTime")
	}
	t1, err := time.Parse(TimeLayout, (*bt.data)[i][timeIndex])
	if err != nil {
		panic(err)
	}
	t2, err := time.Parse(TimeLayout, (*bt.data)[j][timeIndex])
	if err != nil {
		panic(err)
	}
	return t1.Before(t2)
}

// SortByTime returns a pointer to a new RecordSet sorted in ascending order
// by BaseDateTime.
// NOTE: RECORDSETS ARE AN ON-DISK DATA STRUCTURE BUT SORTING IS AN IN-MEMORY
// ACTIVITY THAT USES THE STANDARD SORT PACKAGE.  THEREFORE SORTING
// REQUIRES LOADING THE ENTIRE RECORDSET INTO MEMORY AND HAS ONLY BEEN TESTED
// ON RECORDSETS OF ABOUT A MILLION RECORDS.
func (rs *RecordSet) SortByTime() (*RecordSet, error) {
	rs2 := NewRecordSet()
	rs2.SetHeaders(rs.Headers())

	bt, err := NewByTimestamp(rs)
	if err != nil {
		return nil, fmt.Errorf("sortbytime: %v", err)
	}

	sort.Sort(bt)

	// Write the reports to the new RecordSet
	// NOTE: Headers are written only when the RecordSet is saved to disk
	written := 0
	for _, rec := range *bt.data {
		rs2.Write(rec)
		written++
		if written%flushThreshold == 0 {
			err := rs2.Flush()
			if err != nil {
				return nil, fmt.Errorf("sortbytime: flush error writing to new recordset: %v", err)
			}
		}
	}
	err = rs2.Flush()
	if err != nil {
		return nil, fmt.Errorf("sortbytime: flush error writing to new recordset: %v", err)
	}

	return rs2, nil
}

// Unexported loadRecords reads the RecordSet into memory and returns a
// *[]Record and any error that occurred.  If err is non-nil then loadRecords
// returns nil for the *[]Record
func (rs *RecordSet) loadRecords() (*[]Record, error) {
	recs := new([]Record)

	record := new(Record)
	for {
		var err error
		record, err = rs.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		*recs = append(*recs, *record)
	}
	return recs, nil
}

// Headers is the headers row of an AIS csv file.
type Headers struct {
	// fields is an encapsulated []string that cannot be altered by
	// package users. It is the read only values from the first line of
	// an ais.RecordSet and is initialized with ais.NewRecordSet.
	fields []string

	// dictionary is a map[fieldname]description composed of string values
	// usually created from a JSON file that contains
	// Definition structs for each of the fields in the set of ais.Headers.
	dictionary map[string]string
}

// NewHeaders returns a new set of Headers
func NewHeaders(fields []string, defs []Definition) Headers {
	var h Headers
	h.fields = fields
	if defs != nil {
		dict := make(map[string]string)
		for _, d := range defs {
			dict[d.Fieldname] = d.Description
		}
		h.dictionary = dict
	}
	return h
}

// Contains returns the index of a specific header.  This provides
// a nice syntax ais.Headers.Contains("LAT") to ensure
// an ais.Record contains a specific field.  If the Headers do not
// contain the requested field ok is false.
func (h Headers) Contains(field string) (i int, ok bool) {
	for i, s := range h.fields {
		if s == field {
			return i, true
		}
	}
	return 0, false
}

// String satisfies the fmt.Stringer interface for Headers.
// If a dictionary has been provided then it prints the headers and
// their definitions.
func (h Headers) String() string {
	const pad = ' ' //padding character for prety print

	b := new(bytes.Buffer)
	w := tabwriter.NewWriter(b, 0, 0, 1, pad, 0)
	dictionaryPresent := false
	if h.dictionary != nil {
		dictionaryPresent = true
	}

	// Load the JSON field descriptions and create a map from them if
	// the Headers set has a dictionary file

	// For each header pretty print its name and description
	for i, header := range h.fields {
		header = strings.TrimSpace(header)
		if d, ok := h.dictionary[header]; ok {
			fmt.Fprintf(w, "%2d\t%s:\t%s\n", i, header, d)
		} else {
			defString := ""
			if dictionaryPresent {
				defString = "No definition in dictionary."
			}
			fmt.Fprintf(w, "%2d\t%s:\t%s\n", i, header, defString)
		}
	}
	w.Flush()

	return b.String()
}

// Record wraps the return value from a csv.Reader because many publicly
// available data sources provide AIS records in large csv files. The Record
// type and its associate methods allow clients of the package to deal
// directly with the abtraction of individual AIS records and handle the
// csv file read/write operations internally.
type Record []string

// Hash returns a 64 bit hash/fnv of the Record
func (r Record) Hash() uint64 {
	var h64 hash.Hash64
	h64 = fnv.New64a()
	h64.Write(r.Data())
	return h64.Sum64()
}

// Data returns the underlying []string in a Record as a []byte
func (r Record) Data() []byte {
	var b bytes.Buffer
	b.WriteString(strings.Join([]string(r), ","))
	b.WriteString("\n")
	return b.Bytes()
}

// Distance calculates the haversine distance between two AIS records that
// contain a latitude and longitude measurement identified by their index
// number in the Record slice.
func (r Record) Distance(r2 Record, latIndex, lonIndex int) (nm float64, err error) {
	latP, _ := r.ParseFloat(latIndex)
	lonP, _ := r.ParseFloat(lonIndex)
	latQ, _ := r2.ParseFloat(latIndex)
	lonQ, _ := r2.ParseFloat(lonIndex)
	p := haversine.Coord{Lat: latP, Lon: lonP}
	q := haversine.Coord{Lat: latQ, Lon: lonQ}
	nm = haversine.Distance(p, q)
	return nm, nil
}

// Equals supports comparison testing of two Headers sets. Because
// a data dictionary for the Headers is a nice addition to a RecordSet
// but not necessary for data science work in general, Equals does not
// check to make sure both sets of Headers have matching dictrionaries.
// That said, if one set of Headers does have a dictionary the set
// being compared must also have a dictionary even if the data in those
// two dictionaries is not compared for equality.
func (h Headers) Equals(h2 Headers) bool {
	if (h.dictionary == nil) != (h2.dictionary == nil) {
		return false
	}
	if (h.fields == nil) != (h2.fields == nil) {
		return false
	}

	if len(h.fields) != len(h2.fields) {
		return false
	}

	for i, f := range h.fields {
		if f != h2.fields[i] {
			return false
		}
	}

	return true
}

// ParseFloat wraps strconv.ParseFloat with a method to return a
// float64 from the index value of a field in the AIS Record.
// Useful for getting a LAT, LON, SOG or other numeric value
// from an ais.Record.
func (r Record) ParseFloat(index int) (float64, error) {
	f, err := strconv.ParseFloat(r[index], 64)
	if err != nil {
		return 0, err
	}
	return f, nil
}

// ParseInt wraps strconv.ParseInt with a method to return an
// Int64 from the index value of a field in the AIS Record.
// Useful for getting int values from the Records such as MMSI
// and IMO number.
func (r Record) ParseInt(index int) (int64, error) {
	i, err := strconv.ParseInt(r[index], 10, 64)
	if err != nil {
		return 0, err
	}
	return i, nil
}

// ParseTime wraps time.Parse with a method to return a time.Time
// from the index value of a field in the AIS Record.
// Useful for converting the BaseDateTime from the Record.
// NOTE: FUTURE VERSIONS OF THIS METHOD SHOULD NOT RELY ON A PACKAGE
// CONSTANT FOR THE LAYOUT FIELD. THIS FIELD SHOULD BE INFERRED FROM
// A LIST OF FORMATS SEEN IN COMMON DATASOURCES.
func (r Record) ParseTime(index int) (time.Time, error) {
	t, err := time.Parse(TimeLayout, r[index])
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// Parse converts the string record values into an ais.Report.  It
// takes a set of headers as arguments to identify the fields in
// the Record.
// NOTE 1: FUTURE VERSIONS MAY ALSO RETURN A CORRELATION STRUCT SO
// USERS CAN SEE THE FIELD NAMES THAT WERE USED TO MAKE ASSIGNMENTS
// TO THE REPORT VALUES.  THIS WOULD BE HELPFUL WHEN THERE ARE MULTIPLE
// STRING NAMES TO REPRESENT THE SAME RECORD FIELD.  FOR EXAMPLE, SOME
// DATASETS USE "TIME" INSTEAD OF THE MARINECADASTRE USE OF THE
// FIELD NAME "BASEDATETIME" BUT BOTH SHOULD MAP TO THE "TIMESTAMP" FIELD
// OF REPORT.
// NOTE 2: FUTURE VERSION OF THIS METHOD SHOULD ITERATE OVER THE REPORT
// STRUCT AND FIND THE REQUIRED FIELDS, NOT RELY ON THE HARDCODED VERSION
// PRESENTED IN THE FIRST FEW LINES OF THIS FUNCTION WHERE I HAVE A
// MINIMALLY VIABLE IMPLEMENTATION.
// func (r Record) Parse(h Headers) (Report, error) {
// 	requiredFields := []string{"MMSI", "BaseDateTime", "LAT", "LON"}
// 	fields := make(map[string]int)

// 	for _, field := range requiredFields {
// 		j, ok := h.Contains(field)
// 		if !ok {
// 			return Report{}, fmt.Errorf("record parse: passed headers does not contain required field %s", field)
// 		}
// 		fields[field] = j
// 	}
// 	mmsi, err := r.ParseInt(fields["MMSI"])
// 	if err != nil {
// 		return Report{}, fmt.Errorf("record parse: unable to parse MMSI: %s", err)
// 	}
// 	t, err := r.ParseTime(fields["BaseDateTime"])
// 	if err != nil {
// 		return Report{}, fmt.Errorf("record parse: unable to parse BaseDateTime: %s", err)
// 	}
// 	lat, err := r.ParseFloat(fields["LAT"])
// 	if err != nil {
// 		return Report{}, fmt.Errorf("record parse: unable to parse LAT: %s", err)
// 	}
// 	lon, err := r.ParseFloat(fields["LON"])
// 	if err != nil {
// 		return Report{}, fmt.Errorf("record parse: unable to parse LON: %s", err)
// 	}

// 	return Report{
// 		MMSI:      mmsi,
// 		Lat:       lat,
// 		Lon:       lon,
// 		Timestamp: t,
// 	}, nil

// }

// Report is the converted string data from an ais.Record into a series
// of typed values suitable for data analytics.
// NOTE: THIS SET OF FIELDS WILL EVOLVE OVER TIME TO SUPPORT A LARGER
// SET OF USE CASES AND ANALYTICS.  DO NOT RELY ON THE ORDER OF THE
// FIELDS IN THIS TYPE.
// type Report struct {
// 	MMSI      int64
// 	Lat       float64
// 	Lon       float64
// 	Timestamp time.Time
// 	data      []interface{}
// }

// Data returns the Report fields in a slice of interface values.
// func (rep Report) Data() []interface{} {
// 	rep.data = []interface{}{
// 		int64(rep.MMSI),
// 		time.Time(rep.Timestamp),
// 		float64(rep.Lat),
// 		float64(rep.Lon),
// 	}
// 	return rep.data
// }
