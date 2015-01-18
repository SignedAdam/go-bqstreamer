package main

import (
	"fmt"
	"sort"
	"time"

	bigquery "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
)

// A BigQueryStreamer is a BigQuery streamer, queuing rows and streaming to
// BigQuery in bulk by calling InsertAll().
type BigQueryStreamer struct {
	// BigQuery client connection.
	service *bigquery.Service

	// Upon invokeing Start() streamer will fetch rows from this channel,
	// and queue it into an internal rows queue.
	rowChannel chan *row

	// Internal list to queue rows for stream insert.
	rows []*row

	// Rows index to queue next row into.
	// TODO using a row index is probably not the best way.
	// Maybe we should instead create a slice with len = 0, cap = 500 and use
	// len() instead.
	rowIndex int // 0

	// Max delay between flushes to BigQuery.
	maxDelay time.Duration

	// Sleep delay after a rejected insert and before retry.
	sleepBeforeRetry time.Duration

	// Maximum retry insert attempts for non-rejected row insert errors.
	// e.g. GoogleAPI HTTP errors, generic HTTP errors, etc.
	maxRetryInsert int

	// Shutdown channel to stop Start() execution.
	stopChannel chan bool

	// Errors are reported to this channel.
	Errors chan error

	// The following functions can be overriden for unit testing.

	// Start read-queue-stream loop function.
	Start func()

	// Stop read-queue-stream loop function.
	Stop func()

	// Flush to BigQuery function.
	flush func()

	// Insert all tables to bigquery function.
	insertAll func()

	// Insert table to BigQuery function.
	insertTable func(projectId, datasetId, tableId string, t table) (r *bigquery.TableDataInsertAllResponse, err error)
}

// NewBigQueryStreamer returns a new BigQueryStreamer.
func NewBigQueryStreamer(
	service *bigquery.Service,
	maxRows int,
	maxDelay time.Duration,
	sleepBeforeRetry time.Duration,
	maxRetryInsert int) (b *BigQueryStreamer, err error) {
	// TODO add testing for nil bigquery.Service (this will intervene with tests though,
	// maybe find a way to mock this type somehow?)

	// TODO maybe return error if maxRows > 500?

	if maxRows <= 0 {
		err = fmt.Errorf("maxRows must be positive int > 0")
		return
	}

	if maxDelay <= 0*time.Nanosecond {
		err = fmt.Errorf("maxDelay must be positive time.Duration >= 1 * time.Nanosecond")
		return
	}

	if sleepBeforeRetry <= 0*time.Nanosecond {
		err = fmt.Errorf("sleepBeforeRetry must be positive time.Duration >= 1 * time.Nanosecond")
		return
	}

	if maxRetryInsert < 0 {
		err = fmt.Errorf("maxRetryInsert must be non-negative int >= 0")
		return
	}

	b = &BigQueryStreamer{
		service:          service,
		rowChannel:       make(chan *row, maxRows),
		rows:             make([]*row, maxRows),
		rowIndex:         0,
		maxDelay:         maxDelay,
		sleepBeforeRetry: sleepBeforeRetry,
		maxRetryInsert:   maxRetryInsert,
		stopChannel:      make(chan bool),
		Errors:           make(chan error, errorBufferSize),
	}

	// Assign function defaults.
	b.Start = b.start
	b.Stop = b.stop
	b.flush = b.flushToBigQuery
	b.insertAll = b.insertAllToBigQuery
	b.insertTable = b.insertTableToBigQuery

	return
}

// startStreamer infinitely reads rows from rowChannel and queues them internaly.
// It flushes to BigQuery when queue is filled (according to maxRows) or timer has expired (according to maxDelay).
//
// This function is assigned to BigQueryStreamer.Start member.
// It is overridable so we can test BigQueryMultiStreamer without actually
// starting the streamers.
//
// Note the read-insert-flush loop will never stop, so this function should be
// executed in a goroutine, and stopped via calling Stop().
func (b *BigQueryStreamer) start() {
	t := time.NewTimer(b.maxDelay)
	toStop := false
	for {
		// Flush and reset timer when one of the following signals (channels) fire:
		select {
		case toStop = <-b.stopChannel:
		case <-t.C:
		case r := <-b.rowChannel:
			// Insert row to queue.
			b.rows[b.rowIndex] = r
			b.rowIndex++

			// Don't flush if rows queue isn't full.
			if b.rowIndex < len(b.rows) {
				continue
			}
		}

		b.flush()

		if !toStop {
			t.Reset(b.maxDelay)
		} else {
			t.Stop()
			return
		}
	}
}

// stopStreamer sends a stop message to stop channel, causing Start() infinite loop to stop.
func (b *BigQueryStreamer) stop() {
	b.stopChannel <- true
}

// flushAll streams all queued rows to BigQuery and resets rows queue by
// creating a new queue.
//
// This function is assigned to BigQueryStreamer.flush member.
// It is overridable so we can test BigQueryStreamer without actually flushing
// to BigQuery.
//
// TODO Consider making this public. If so, we should use a mutex to lock the object,
// otherwise if the object is running in another goroutine it can call this in parallel.
func (b *BigQueryStreamer) flushToBigQuery() {
	b.insertAll()

	// Init (reset) a new rows queue - clear old one and re-allocate.
	b.rows = nil
	b.rows = make([]*row, len(b.rows))
	b.rowIndex = 0
}

// QueueRow sends a single row to the row channel, which will be queued and inserted in bulk with other queued rows.
func (b *BigQueryStreamer) QueueRow(projectID, datasetID, tableID string, jsonRow map[string]bigquery.JsonValue) {
	b.rowChannel <- &row{projectID, datasetID, tableID, jsonRow}
}

// BigQuery data types.
type project map[string]dataset
type dataset map[string]table
type table []tableRow
type tableRow map[string]bigquery.JsonValue

// createTableIfNotExists initializes given project, dataset, and table
// in project map if they haven't been initialized yet.
func createTableIfNotExists(ps map[string]project, p, d, t string) {
	// Create table's project if non-existent.
	if _, ok := ps[p]; !ok {
		ps[p] = project{}
	}

	// Create table's dataset if non-existent.
	if _, ok := ps[p][d]; !ok {
		ps[p][d] = dataset{}
	}

	// Create table if non-existent.
	if _, ok := ps[p][d][t]; !ok {
		ps[p][d][t] = table{}
	}
}

// insertAllToBigQuery inserts all rows from all tables to BigQuery.
// Each table is inserted separately, according to BigQuery's requirements.
// Insert errors are reported to error channel.
func (b *BigQueryStreamer) insertAllToBigQuery() {
	// Sort rows by project->dataset->table
	// Necessary because each InsertAll() has to be for a single table.
	ps := map[string]project{}
	for i := 0; i < b.rowIndex; i++ {
		r := b.rows[i]

		p, d, t := r.projectID, r.datasetID, r.tableID

		// Create project, dataset and table if uninitalized.
		createTableIfNotExists(ps, p, d, t)

		// Assign row to table.
		ps[p][d][t] = append(ps[p][d][t], r.data)
	}

	// Stream insert each table to BigQuery.
	for pID, p := range ps {
		for dID, d := range p {
			for tID := range d {
				// Insert to a single table in bulk, and retry insert on certain errors.
				// Keep on retrying until successful.
				numRetries := 0
				for {
					numRetries++
					if numRetries > b.maxRetryInsert {
						b.Errors <- fmt.Errorf(
							"Insert table %s retried %d times, dropping insert and moving on",
							tID, numRetries)
						break
					} else if len(d[tID]) == 0 {
						b.Errors <- fmt.Errorf("All rows from table %s have been filtered, moving on", tID)
						break
					}

					responses, err := b.insertTable(pID, dID, tID, d[tID])

					// Retry on certain HTTP errors.
					if b.shouldRetryInsertAfterError(err) {
						// Retry after HTTP errors usually mean to retry after a certain pause.
						// See the following link for more info:
						// https://cloud.google.com/bigquery/troubleshooting-errors
						time.Sleep(b.sleepBeforeRetry)
						continue
					}

					// Retry if insert was rejected due to bad rows.
					// Occurence of bad rows do not count against retries,
					// as this means we're trying to insert bad data to BigQuery.
					rejectedRows := b.filterRejectedRows(responses, pID, dID, tID, d)
					if len(rejectedRows) > 0 {
						numRetries--
						continue
					}

					// If we reached here it means insert was successful,
					// so retry isn't necessary.
					// Thus, break from the "retry insert" loop.
					break
				}
			}
		}
	}
}

// insertTableToBigQuery inserts a single table to BigQuery using BigQuery's InsertAll request.
//
// This function is assigned to BigQueryStreamer.insertTable member.
// It is overridable so we can test BigQueryStreamer without actually inserting anythin to BigQuery.
func (b *BigQueryStreamer) insertTableToBigQuery(projectID, datasetID, tableID string, t table) (
	r *bigquery.TableDataInsertAllResponse, err error) {
	// Convert all rows to bigquery table rows.
	rows := make([]*bigquery.TableDataInsertAllRequestRows, len(t))
	for i, json := range t {
		rows[i] = &bigquery.TableDataInsertAllRequestRows{Json: json}
	}

	// Generate request, tabledata and send.
	request := bigquery.TableDataInsertAllRequest{Kind: "bigquery#tableDataInsertAllRequest", Rows: rows}

	// TODO might be better to cache table services somehow, instead of re-creating them on every flush.
	tableService := bigquery.NewTabledataService(b.service)

	r, err = tableService.InsertAll(projectID, datasetID, tableID, &request).Do()

	return
}

// shouldRetryInsertAfterError checks for insert HTTP response errors,
// and returns true if insert should be retried.
// See the following url for more info:
// https://cloud.google.com/bigquery/troubleshooting-errors
func (b *BigQueryStreamer) shouldRetryInsertAfterError(err error) (shouldRetry bool) {
	shouldRetry = false

	if err != nil {
		// Retry on GoogleAPI HTTP server error (503).
		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code == 503 {
			shouldRetry = true
		}

		// Log and don't retry for any other response codes,
		// or if not a Google API response at all.
		b.Errors <- err
	}

	return
}

// filterRejectedRows checks for per-row responses,
// removes rejected rows given table, and returns index slice of rows removed.
//
// Rows are rejected if BigQuery insert response marked them as any string
// other than "stopped".  See the following url for further info:
// https://cloud.google.com/bigquery/streaming-data-into-bigquery#troubleshooting
func (b *BigQueryStreamer) filterRejectedRows(
	responses *bigquery.TableDataInsertAllResponse,
	pID, dID, tID string,
	d map[string]table) (rowsToFilter []int64) {

	// Go through all rows and rows' errors, and remove rejected (bad) rows.
	if responses != nil {
		for _, rowErrors := range responses.InsertErrors {
			// We use a sanity switch to make sure we don't append
			// the same row to be deleted more than once.
			filter := false

			// Each row can have several errors.
			// Go through each of these, and remove row if one of these errors != "stopped".
			// Also log all non-"stopped" errors on the fly.
			for _, rowErrorPtr := range rowErrors.Errors {
				rowError := *rowErrorPtr

				// Mark invalid rows to be deleted.
				if rowError.Reason != "stopped" {
					if !filter {
						rowsToFilter = append(rowsToFilter, rowErrors.Index)
						filter = true
					}

					// Log all errors besides "stopped" ones.
					b.Errors <- fmt.Errorf(
						"%s.%s.%s.row[%d]: %s in %s: %s: %s",
						pID, dID, tID,
						rowErrors.Index,
						rowError.Reason, rowError.Location, rowError.Message,
						d[tID][rowErrors.Index])
				}
			}
		}
	}

	// Remove accumulated rejected rows from table (if there were any).
	if len(rowsToFilter) > 0 {
		// Replace modified table instead of original.
		// This is necessary because original table's slice has a different len().
		//
		// XXX is this ok?
		d[tID] = b.filterRowsFromTable(rowsToFilter, d[tID])
	}

	return
}

// "sort"-compliant int64 class. Used for sorting in removeRows() below.
// This is necessary because for some reason package sort doesn't support sorting int64 slices.
type int64Slice []int64

func (a int64Slice) Len() int           { return len(a) }
func (a int64Slice) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a int64Slice) Less(i, j int) bool { return a[i] < a[j] }

// removeRows removes rows by given indexes from given table,
// and returns the filtered table.
// Table filtering is done in place, thus the original table is also changed.
//
// The table is returned in addition to mutating the given table argument for idiom's sake.
func (b *BigQueryStreamer) filterRowsFromTable(indexes []int64, t table) table {
	// Deletion is done in-place, so we copy & sort given indexes,
	// then delete in reverse order. Reverse order is necessary for not
	// breaking the order of elements to remove in the slice.
	//
	// Create a copy of given index slice in order to not modify the outer index slice.
	is := append([]int64(nil), indexes...)
	sort.Sort(sort.Reverse(int64Slice(is)))

	for _, i := range is {
		// Set bad row to be garbage collected.
		t[i] = nil

		// Switch old row in-place with last row of slice.
		t[i], t = t[len(t)-1], t[:len(t)-1]
	}

	// Return the same table.
	return t
}
