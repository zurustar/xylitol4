package userdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

func init() {
	sql.Register("sqlite", &memoryDriver{databases: make(map[string]*memoryDatabase)})
}

type memoryDriver struct {
	mu        sync.Mutex
	databases map[string]*memoryDatabase
}

func (d *memoryDriver) Open(name string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	db := d.databases[name]
	if db == nil {
		db = newMemoryDatabase()
		d.databases[name] = db
	}
	return &memoryConn{db: db}, nil
}

type memoryConn struct {
	db *memoryDatabase
}

func (c *memoryConn) Prepare(query string) (driver.Stmt, error) {
	return &memoryStmt{db: c.db, query: query}, nil
}

func (c *memoryConn) Close() error                   { return nil }
func (c *memoryConn) Begin() (driver.Tx, error)      { return nil, errors.New("transactions not supported") }
func (c *memoryConn) Ping(ctx context.Context) error { return nil }

func (c *memoryConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.exec(query, args)
}

func (c *memoryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.query(query, args)
}

func (c *memoryConn) exec(query string, args []driver.NamedValue) (driver.Result, error) {
	stmt, err := parseSQL(query)
	if err != nil {
		return nil, err
	}
	switch s := stmt.(type) {
	case createTableStmt:
		if err := c.db.createTable(s); err != nil {
			return nil, err
		}
		return memoryResult{}, nil
	case insertStmt:
		bound, err := bindInsertValues(s.values, args)
		if err != nil {
			return nil, err
		}
		s.values = bound
		if err := c.db.insertRow(s); err != nil {
			return nil, err
		}
		return memoryResult{rowsAffected: int64(len(s.values))}, nil
	case updateStmt:
		setValues, whereValues, err := bindUpdateArgs(s, args)
		if err != nil {
			return nil, err
		}
		affected, err := c.db.updateRows(s, setValues, whereValues)
		if err != nil {
			return nil, err
		}
		return memoryResult{rowsAffected: affected}, nil
	case deleteStmt:
		whereValues, err := bindDeleteArgs(s, args)
		if err != nil {
			return nil, err
		}
		affected, err := c.db.deleteRows(s, whereValues)
		if err != nil {
			return nil, err
		}
		return memoryResult{rowsAffected: affected}, nil
	case selectStmt:
		// SELECT executed via Exec is not supported
		return nil, fmt.Errorf("select statements require Query")
	default:
		return nil, fmt.Errorf("unsupported statement type %T", stmt)
	}
}

func (c *memoryConn) query(query string, args []driver.NamedValue) (driver.Rows, error) {
	stmt, err := parseSQL(query)
	if err != nil {
		return nil, err
	}
	sel, ok := stmt.(selectStmt)
	if !ok {
		return nil, fmt.Errorf("statement is not a SELECT")
	}
	values := make([]string, len(args))
	for i, arg := range args {
		values[i] = fmt.Sprint(arg.Value)
	}
	rows := c.db.selectRows(sel, values)
	data := make([][]driver.Value, len(rows))
	for i, row := range rows {
		record := make([]driver.Value, len(row))
		for j, value := range row {
			record[j] = value
		}
		data[i] = record
	}
	return &memoryRows{columns: sel.columns, data: data}, nil
}

type memoryStmt struct {
	db    *memoryDatabase
	query string
}

func (s *memoryStmt) Close() error  { return nil }
func (s *memoryStmt) NumInput() int { return -1 }

func (s *memoryStmt) Exec(args []driver.Value) (driver.Result, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return (&memoryConn{db: s.db}).exec(s.query, named)
}

func (s *memoryStmt) Query(args []driver.Value) (driver.Rows, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return (&memoryConn{db: s.db}).query(s.query, named)
}

type memoryRows struct {
	columns []string
	data    [][]driver.Value
	idx     int
}

func (r *memoryRows) Columns() []string { return append([]string(nil), r.columns...) }

func (r *memoryRows) Close() error { return nil }

func (r *memoryRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	row := r.data[r.idx]
	for i := range dest {
		if i < len(row) {
			dest[i] = row[i]
		} else {
			dest[i] = nil
		}
	}
	r.idx++
	return nil
}

type memoryResult struct {
	rowsAffected int64
}

func (r memoryResult) LastInsertId() (int64, error) { return 0, errors.New("not supported") }
func (r memoryResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type memoryDatabase struct {
	mu     sync.RWMutex
	tables map[string]*memoryTable
}

type memoryTable struct {
	columns       []string
	rows          []map[string]string
	autoIncrement int64
}

func newMemoryDatabase() *memoryDatabase {
	return &memoryDatabase{tables: make(map[string]*memoryTable)}
}

func (db *memoryDatabase) createTable(stmt createTableStmt) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.tables[stmt.name]; ok {
		return fmt.Errorf("table %s already exists", stmt.name)
	}
	db.tables[stmt.name] = &memoryTable{columns: stmt.columns}
	return nil
}

func (db *memoryDatabase) insertRow(stmt insertStmt) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	table, ok := db.tables[stmt.table]
	if !ok {
		return fmt.Errorf("table %s does not exist", stmt.table)
	}
	for _, vals := range stmt.values {
		if len(vals) != len(stmt.columns) {
			return fmt.Errorf("column count mismatch")
		}
		row := make(map[string]string, len(stmt.columns))
		for i, col := range stmt.columns {
			row[col] = vals[i]
		}
		if table.hasColumn("id") {
			if raw, ok := row["id"]; ok {
				if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > table.autoIncrement {
					table.autoIncrement = n
				}
			} else {
				table.autoIncrement++
				row["id"] = strconv.FormatInt(table.autoIncrement, 10)
			}
		}
		table.rows = append(table.rows, row)
	}
	return nil
}

func (t *memoryTable) hasColumn(name string) bool {
	for _, col := range t.columns {
		if strings.EqualFold(col, name) {
			return true
		}
	}
	return false
}

func (db *memoryDatabase) updateRows(stmt updateStmt, setValues, whereValues []string) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	table, ok := db.tables[stmt.table]
	if !ok {
		return 0, fmt.Errorf("table %s does not exist", stmt.table)
	}
	if len(stmt.setColumns) != len(setValues) {
		return 0, fmt.Errorf("update column/value mismatch")
	}
	where := make(map[string]string, len(stmt.whereColumns))
	for i, col := range stmt.whereColumns {
		if i < len(whereValues) {
			where[col] = whereValues[i]
		}
	}
	var affected int64
	for _, row := range table.rows {
		if !rowMatches(row, where) {
			continue
		}
		for i, col := range stmt.setColumns {
			row[col] = setValues[i]
		}
		affected++
	}
	return affected, nil
}

func (db *memoryDatabase) deleteRows(stmt deleteStmt, whereValues []string) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	table, ok := db.tables[stmt.table]
	if !ok {
		return 0, fmt.Errorf("table %s does not exist", stmt.table)
	}
	where := make(map[string]string, len(stmt.whereColumns))
	for i, col := range stmt.whereColumns {
		if i < len(whereValues) {
			where[col] = whereValues[i]
		}
	}
	var affected int64
	kept := make([]map[string]string, 0, len(table.rows))
	for _, row := range table.rows {
		if rowMatches(row, where) {
			affected++
			continue
		}
		kept = append(kept, row)
	}
	table.rows = kept
	return affected, nil
}

func (db *memoryDatabase) selectRows(stmt selectStmt, args []string) [][]string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	table, ok := db.tables[stmt.table]
	if !ok {
		return nil
	}
	var rows [][]string
	argMap := make(map[string]string, len(stmt.whereColumns))
	for i, col := range stmt.whereColumns {
		if i < len(args) {
			argMap[col] = args[i]
		}
	}
	requestedColumns := stmt.columns
	if len(requestedColumns) == 0 {
		requestedColumns = table.columns
	}
	for _, stored := range table.rows {
		if len(argMap) > 0 {
			matched := true
			for col, expected := range argMap {
				if stored[col] != expected {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
		}
		row := make([]string, len(requestedColumns))
		for i, col := range requestedColumns {
			row[i] = stored[col]
		}
		rows = append(rows, row)
		if stmt.limitOne && len(rows) == 1 {
			break
		}
	}
	return rows
}

type createTableStmt struct {
	name    string
	columns []string
}

type insertStmt struct {
	table   string
	columns []string
	values  [][]string
}

type updateStmt struct {
	table        string
	setColumns   []string
	setValues    []string
	whereColumns []string
}

type deleteStmt struct {
	table        string
	whereColumns []string
}

type selectStmt struct {
	columns      []string
	table        string
	whereColumns []string
	limitOne     bool
}

var (
	createTableRegex = regexp.MustCompile(`(?is)^CREATE\s+TABLE\s+([a-zA-Z_][a-zA-Z0-9_]*)\s*\((.+)\)$`)
	insertRegex      = regexp.MustCompile(`(?is)^INSERT\s+INTO\s+([a-zA-Z_][a-zA-Z0-9_]*)\s*\(([^)]+)\)\s+VALUES\s*(.+)$`)
)

func parseSQL(query string) (interface{}, error) {
	trimmed := strings.TrimSpace(query)
	trimmed = strings.TrimSuffix(trimmed, ";")
	if strings.HasPrefix(strings.ToUpper(trimmed), "CREATE TABLE") {
		return parseCreateTable(trimmed)
	}
	if strings.HasPrefix(strings.ToUpper(trimmed), "INSERT INTO") {
		return parseInsert(trimmed)
	}
	if strings.HasPrefix(strings.ToUpper(trimmed), "UPDATE") {
		return parseUpdate(trimmed)
	}
	if strings.HasPrefix(strings.ToUpper(trimmed), "DELETE FROM") {
		return parseDelete(trimmed)
	}
	if strings.HasPrefix(strings.ToUpper(trimmed), "SELECT") {
		return parseSelect(trimmed)
	}
	return nil, fmt.Errorf("unsupported SQL: %s", query)
}

func parseCreateTable(query string) (createTableStmt, error) {
	matches := createTableRegex.FindStringSubmatch(query)
	if len(matches) != 3 {
		return createTableStmt{}, fmt.Errorf("invalid CREATE TABLE syntax")
	}
	name := matches[1]
	colsSegment := matches[2]
	colDefs := splitComma(colsSegment)
	columns := make([]string, 0, len(colDefs))
	for _, def := range colDefs {
		def = strings.TrimSpace(def)
		if def == "" {
			continue
		}
		fields := strings.Fields(def)
		if len(fields) == 0 {
			continue
		}
		columns = append(columns, fields[0])
	}
	if len(columns) == 0 {
		return createTableStmt{}, fmt.Errorf("no columns defined")
	}
	return createTableStmt{name: name, columns: columns}, nil
}

func parseInsert(query string) (insertStmt, error) {
	matches := insertRegex.FindStringSubmatch(query)
	if len(matches) != 4 {
		return insertStmt{}, fmt.Errorf("invalid INSERT syntax")
	}
	table := matches[1]
	columns := splitComma(matches[2])
	for i, col := range columns {
		columns[i] = strings.TrimSpace(col)
	}
	valuesPart := strings.TrimSpace(matches[3])
	if !strings.HasPrefix(valuesPart, "(") {
		return insertStmt{}, fmt.Errorf("invalid INSERT values")
	}
	tuples := splitTuples(valuesPart)
	values := make([][]string, 0, len(tuples))
	for _, tuple := range tuples {
		fields := splitComma(tuple)
		if len(fields) != len(columns) {
			return insertStmt{}, fmt.Errorf("mismatched column count in values")
		}
		row := make([]string, len(columns))
		for i, value := range fields {
			row[i] = unquote(strings.TrimSpace(value))
		}
		values = append(values, row)
	}
	return insertStmt{table: table, columns: columns, values: values}, nil
}

func parseUpdate(query string) (updateStmt, error) {
	upper := strings.ToUpper(query)
	if !strings.HasPrefix(upper, "UPDATE ") {
		return updateStmt{}, fmt.Errorf("invalid UPDATE syntax")
	}
	remainder := strings.TrimSpace(query[len("UPDATE "):])
	setIdx := strings.Index(strings.ToUpper(remainder), " SET ")
	if setIdx == -1 {
		return updateStmt{}, fmt.Errorf("missing SET clause")
	}
	table := strings.TrimSpace(remainder[:setIdx])
	remainder = strings.TrimSpace(remainder[setIdx+len(" SET "):])
	whereIdx := strings.Index(strings.ToUpper(remainder), " WHERE ")
	assignments := remainder
	whereClause := ""
	if whereIdx != -1 {
		assignments = strings.TrimSpace(remainder[:whereIdx])
		whereClause = strings.TrimSpace(remainder[whereIdx+len(" WHERE "):])
	}
	parts := splitComma(assignments)
	setColumns := make([]string, 0, len(parts))
	setValues := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pieces := strings.SplitN(part, "=", 2)
		if len(pieces) != 2 {
			continue
		}
		setColumns = append(setColumns, strings.TrimSpace(pieces[0]))
		setValues = append(setValues, strings.TrimSpace(pieces[1]))
	}
	if len(setColumns) == 0 {
		return updateStmt{}, fmt.Errorf("no columns to update")
	}
	whereColumns := []string{}
	if whereClause != "" {
		whereColumns = parseWhere(whereClause)
	}
	return updateStmt{table: table, setColumns: setColumns, setValues: setValues, whereColumns: whereColumns}, nil
}

func parseDelete(query string) (deleteStmt, error) {
	upper := strings.ToUpper(query)
	if !strings.HasPrefix(upper, "DELETE FROM ") {
		return deleteStmt{}, fmt.Errorf("invalid DELETE syntax")
	}
	remainder := strings.TrimSpace(query[len("DELETE FROM "):])
	table := remainder
	whereColumns := []string{}
	if idx := strings.Index(strings.ToUpper(remainder), " WHERE "); idx != -1 {
		table = strings.TrimSpace(remainder[:idx])
		clause := strings.TrimSpace(remainder[idx+len(" WHERE "):])
		whereColumns = parseWhere(clause)
	}
	return deleteStmt{table: table, whereColumns: whereColumns}, nil
}

func bindInsertValues(values [][]string, args []driver.NamedValue) ([][]string, error) {
	bound := make([][]string, len(values))
	argIdx := 0
	for i, tuple := range values {
		row := make([]string, len(tuple))
		for j, val := range tuple {
			if val == "?" {
				if argIdx >= len(args) {
					return nil, fmt.Errorf("missing argument for INSERT placeholder")
				}
				row[j] = fmt.Sprint(args[argIdx].Value)
				argIdx++
			} else {
				row[j] = val
			}
		}
		bound[i] = row
	}
	if argIdx != len(args) {
		return nil, fmt.Errorf("unexpected argument count for INSERT")
	}
	return bound, nil
}

func bindUpdateArgs(stmt updateStmt, args []driver.NamedValue) ([]string, []string, error) {
	setValues := make([]string, len(stmt.setValues))
	argIdx := 0
	for i, raw := range stmt.setValues {
		if raw == "?" {
			if argIdx >= len(args) {
				return nil, nil, fmt.Errorf("missing argument for UPDATE placeholder")
			}
			setValues[i] = fmt.Sprint(args[argIdx].Value)
			argIdx++
		} else {
			setValues[i] = unquote(raw)
		}
	}
	whereValues := make([]string, len(stmt.whereColumns))
	for i := range whereValues {
		if argIdx >= len(args) {
			return nil, nil, fmt.Errorf("missing argument for UPDATE WHERE placeholder")
		}
		whereValues[i] = fmt.Sprint(args[argIdx].Value)
		argIdx++
	}
	if argIdx != len(args) {
		return nil, nil, fmt.Errorf("unexpected argument count for UPDATE")
	}
	return setValues, whereValues, nil
}

func bindDeleteArgs(stmt deleteStmt, args []driver.NamedValue) ([]string, error) {
	whereValues := make([]string, len(stmt.whereColumns))
	if len(args) != len(stmt.whereColumns) {
		return nil, fmt.Errorf("unexpected argument count for DELETE")
	}
	for i := range whereValues {
		whereValues[i] = fmt.Sprint(args[i].Value)
	}
	return whereValues, nil
}

func parseSelect(query string) (selectStmt, error) {
	upper := strings.ToUpper(query)
	fromIdx := strings.Index(upper, " FROM ")
	if fromIdx == -1 {
		return selectStmt{}, fmt.Errorf("missing FROM clause")
	}
	columnsPart := strings.TrimSpace(query[len("SELECT"):fromIdx])
	remainder := strings.TrimSpace(query[fromIdx+len(" FROM "):])
	table := remainder
	whereColumns := []string{}
	limitOne := false
	if idx := strings.Index(strings.ToUpper(remainder), " WHERE "); idx != -1 {
		table = strings.TrimSpace(remainder[:idx])
		remainder = strings.TrimSpace(remainder[idx+len(" WHERE "):])
		if whereEnd := strings.Index(strings.ToUpper(remainder), " LIMIT "); whereEnd != -1 {
			whereClause := strings.TrimSpace(remainder[:whereEnd])
			remainder = strings.TrimSpace(remainder[whereEnd+len(" LIMIT "):])
			limitOne = parseLimit(remainder)
			whereColumns = parseWhere(whereClause)
		} else {
			whereColumns = parseWhere(remainder)
			remainder = ""
		}
	} else if idx := strings.Index(strings.ToUpper(remainder), " LIMIT "); idx != -1 {
		table = strings.TrimSpace(remainder[:idx])
		remainder = strings.TrimSpace(remainder[idx+len(" LIMIT "):])
		limitOne = parseLimit(remainder)
	}
	columns := splitComma(columnsPart)
	for i, col := range columns {
		columns[i] = strings.TrimSpace(col)
	}
	if len(columns) == 1 && columns[0] == "*" {
		// We'll expand at runtime based on table definition
		columns = nil
	}
	return selectStmt{columns: columns, table: table, whereColumns: whereColumns, limitOne: limitOne}, nil
}

func parseLimit(part string) bool {
	part = strings.TrimSpace(part)
	if part == "" {
		return false
	}
	if strings.HasPrefix(part, "(") {
		part = strings.TrimSpace(strings.Trim(part, "()"))
	}
	value, err := strconv.Atoi(part)
	if err != nil {
		return false
	}
	return value == 1
}

func parseWhere(clause string) []string {
	clause = strings.TrimSpace(clause)
	if clause == "" {
		return nil
	}
	conditions := strings.Split(clause, "AND")
	columns := make([]string, 0, len(conditions))
	for _, cond := range conditions {
		cond = strings.TrimSpace(cond)
		parts := strings.Split(cond, "=")
		if len(parts) != 2 {
			continue
		}
		column := strings.TrimSpace(parts[0])
		columns = append(columns, column)
	}
	return columns
}

func splitComma(input string) []string {
	segments := []string{}
	current := strings.Builder{}
	depth := 0
	inQuote := false
	var quote rune
	for _, r := range input {
		switch {
		case r == '\'' || r == '"':
			if inQuote && r == quote {
				inQuote = false
			} else if !inQuote {
				inQuote = true
				quote = r
			}
			current.WriteRune(r)
		case r == '(' && !inQuote:
			depth++
			current.WriteRune(r)
		case r == ')' && !inQuote:
			depth--
			current.WriteRune(r)
		case r == ',' && !inQuote && depth == 0:
			segments = append(segments, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		segments = append(segments, current.String())
	}
	return segments
}

func splitTuples(values string) []string {
	values = strings.TrimSpace(values)
	if values == "" {
		return nil
	}
	tuples := []string{}
	depth := 0
	start := -1
	inQuote := false
	var quote rune
	for i, r := range values {
		switch r {
		case '\'', '"':
			if inQuote && r == quote {
				inQuote = false
			} else if !inQuote {
				inQuote = true
				quote = r
			}
		case '(':
			if !inQuote {
				if depth == 0 {
					start = i + 1
				}
				depth++
			}
		case ')':
			if !inQuote {
				depth--
				if depth == 0 && start >= 0 {
					tuples = append(tuples, values[start:i])
					start = -1
				}
			}
		}
	}
	return tuples
}

func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) || (strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\""))) {
		value = value[1 : len(value)-1]
	}
	value = strings.ReplaceAll(value, "''", "'")
	value = strings.ReplaceAll(value, "\"\"", "\"")
	return value
}

func rowMatches(row map[string]string, conditions map[string]string) bool {
	if len(conditions) == 0 {
		return true
	}
	for col, expected := range conditions {
		if row[col] != expected {
			return false
		}
	}
	return true
}
