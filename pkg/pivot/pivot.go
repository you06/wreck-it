package pivot

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/pingcap/log"
	"github.com/zhouqiang-cl/wreck-it/pkg/connection"
	"github.com/zhouqiang-cl/wreck-it/pkg/executor"
	"github.com/zhouqiang-cl/wreck-it/pkg/generator"
	"go.uber.org/zap"

	"github.com/pingcap/parser/model"
)

type Pivot struct {
	wg       sync.WaitGroup
	Conf     *Config
	DB       *sql.DB
	DBName   string
	Executor *executor.Executor

	Generator
}

func NewPivot(dsn string, DBName string) (*Pivot, error) {
	e, err := executor.New(dsn, "test")
	if err != nil {
		return nil, err
	}
	conf := &Config{
		Dsn:         dsn,
		PrepareStmt: false,
		Hint:        false,
	}
	return &Pivot{
		Conf:      conf,
		DBName:    DBName,
		Executor:  e,
		Generator: Generator{},
	}, nil
}

const (
	tableSQL        = "DESC %s.%s"
	indexSQL        = "SHOW INDEX FROM %s.%s"
	schemaSQL       = "SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE FROM information_schema.tables"
	indexColumnName = "Key_name"
)

func (p *Pivot) Start(ctx context.Context) {
	p.cleanup(ctx)
	p.kickup(ctx)
}

func (p *Pivot) Close() {
	p.wg.Wait()
	p.cleanup(context.Background())
	p.Executor.Close()

}

func (p *Pivot) Init(ctx context.Context) {
	rand.Seed(time.Now().UnixNano())
	p.Tables = make([]Table, 0)

	// Warn: Hard code db name
	tables, err := p.Executor.GetConn().FetchTables(p.DBName)
	if err != nil {
		panic(err)
	}
	for _, i := range tables {
		t := Table{Name: model.NewCIStr(i)}
		t.Columns, err = p.Executor.GetConn().FetchColumns(p.DBName, i)
		if err != nil {
			panic(err)
		}
		idx, err := p.Executor.GetConn().FetchIndexes(p.DBName, i)
		if err != nil {
			panic(err)
		}
		for _, j := range idx {
			t.Indexes = append(t.Indexes, model.NewCIStr(j))
		}
		p.Tables = append(p.Tables, t)
	}
}

func (p *Pivot) prepare(ctx context.Context) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < r.Intn(10)+1; i++ {
		sql, _ := p.Executor.GenerateDDLCreateTable()
		err := p.Executor.Exec(sql.SQLStmt)
		if err != nil {
			log.L().Error("create table failed", zap.String("sql", sql.SQLStmt), zap.Error(err))
		}
	}

	err := p.Executor.ReloadSchema()
	if err != nil {
		log.Error("reload data failed!")
	}
	ddlOpt := &generator.DDLOptions{
		OnlineDDL: true,
		Tables:    []string{},
	}
	for i := 0; i < r.Intn(10); i++ {
		sql, _ := p.Executor.GenerateDDLCreateIndex(ddlOpt)
		fmt.Println(sql)
		err = p.Executor.Exec(sql.SQLStmt)
		if err != nil {
			log.L().Error("create index failed", zap.String("sql", sql.SQLStmt), zap.Error(err))
		}
	}

	for _, table := range p.Executor.GetTables() {
		sql, _ := p.Executor.GenerateDMLInsertByTable(table.Table)
		err = p.Executor.Exec(sql.SQLStmt)
		if err != nil {
			log.L().Error("insert data failed", zap.String("sql", sql.SQLStmt), zap.Error(err))
		}
	}
}

func (p *Pivot) cleanup(ctx context.Context) {
	p.Executor.Exec("drop database if exists " + p.DBName)
	p.Executor.Exec("create database " + p.DBName)
	p.Executor.Exec("use " + p.DBName)
}

func (p *Pivot) kickup(ctx context.Context) {
	p.wg.Add(1)
	p.prepare(ctx)
	p.Init(ctx)

	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
				for {
					p.progress(ctx)
				}
			}
		}

	}()
}

func (p *Pivot) progress(ctx context.Context) {
	// rand one pivot row for one table
	pivotRows, usedTables, err := p.ChoosePivotedRow()
	if err != nil {
		panic(err)
	}
	// generate sql ast tree and
	// generate sql where clause
	selectStmt, columns, err := p.GenSelectStmt(pivotRows, usedTables)
	if err != nil {
		panic(err)
	}
	// execute sql, ensure not null result set
	resultRows, err := p.execSelect(selectStmt)
	if err != nil {
		panic(err)
	}
	// verify pivot row in result row set
	correct := p.verify(pivotRows, columns, resultRows)
	if !correct {
		panic(fmt.Sprintf("data verified failed. pivot rows: %v . result rows: %v . query: %s",
			pivotRows, resultRows, selectStmt,
		))
	}
	fmt.Printf("run one statment [%s] successfully!\n", selectStmt)
	log.Info("run one statment successfully!", zap.String("query", selectStmt))
}

// may move to another struct
func (p *Pivot) ChoosePivotedRow() (map[TableColumn]*connection.QueryItem, []Table, error) {
	result := make(map[TableColumn]*connection.QueryItem)
	count := 1
	if len(p.Tables) > 1 {
		count = Rd(len(p.Tables)-1) + 1
	}
	rand.Shuffle(len(p.Tables), func(i, j int) { p.Tables[i], p.Tables[j] = p.Tables[j], p.Tables[i] })
	usedTables := p.Tables[:count]
	var reallyUsed []Table

	for _, i := range usedTables {
		sql := fmt.Sprintf("SELECT * FROM %s ORDER BY RAND() LIMIT 1;", i.Name)
		exeRes, err := p.execSelect(sql)
		if err != nil {
			panic(err)
		}
		if len(exeRes) > 0 {
			for _, c := range exeRes[0] {
				// panic(fmt.Sprintf("no rows in table %s", i.Name))
				tableColumn := TableColumn{i.Name.O, c.ValType.Name()}
				result[tableColumn] = c
			}
			reallyUsed = append(reallyUsed, i)

		}
	}
	return result, reallyUsed, nil
}

func (p *Pivot) GenSelectStmt(pivotRows map[TableColumn]*connection.QueryItem, usedTables []Table) (string, []TableColumn, error) {
	stmtAst, err := p.selectStmtAst(6, usedTables)
	if err != nil {
		return "", nil, err
	}
	sql, columns, err := p.selectStmt(&stmtAst, usedTables, pivotRows)
	if err != nil {
		return "", nil, err
	}
	return sql, columns, nil
}

func (p *Pivot) ExecAndVerify(stmt string, originRow map[TableColumn]*connection.QueryItem, columns []TableColumn) (bool, error) {
	resultSets, err := p.execSelect(stmt)
	if err != nil {
		return false, err
	}
	res := p.verify(originRow, columns, resultSets)
	return res, nil
}

// may not return string
func (p *Pivot) execSelect(stmt string) ([][]*connection.QueryItem, error) {
	return p.Executor.GetConn().Select(stmt)
}

// TODO implement it
func (p *Pivot) verify(originRow map[TableColumn]*connection.QueryItem, columns []TableColumn, resultSets [][]*connection.QueryItem) bool {
	fmt.Println("=========  ORIGIN ROWS ======")
	for k, v := range originRow {
		fmt.Printf("key: %+v, value: [null: %v, value: %s]\n", k, v.Null, v.ValString)
	}

	fmt.Println("=========  COLUMNS ======")
	for _, c := range columns {
		fmt.Printf("Table: %s, Name: %s\n", c.Table, c.Name)
	}

	for _, row := range resultSets {
		if p.checkRow(originRow, columns, row) {
			return true
		}
	}
	fmt.Printf("=========  DATA ======, count: %d\n", len(resultSets))
	for i, r := range resultSets {
		fmt.Printf("$$$$$$$$$ line %d\n", i)
		for j, c := range r {
			fmt.Printf("  table: %s, field: %s, field: %s, value: %s\n", columns[j].Table, columns[j].Name, c.ValType.Name(), c.ValString)
		}
	}

	fmt.Printf("Verify failed! \n")
	return false
}

func (p *Pivot) checkRow(originRow map[TableColumn]*connection.QueryItem, columns []TableColumn, resultSet []*connection.QueryItem) bool {
	for i, c := range columns {
		fmt.Printf("i: %d, column: %+v, left: %+v, right: %+v", i, c, originRow[c], resultSet[i])
		if !compareQueryItem(originRow[c], resultSet[i]) {
			return false
		}
	}
	return true
}

func compareQueryItem(left *connection.QueryItem, right *connection.QueryItem) bool {
	if left.ValType.Name() != right.ValType.Name() {
		return false
	}
	if left.Null != right.Null {
		return false
	}

	return (left.Null && right.Null) || (left.ValString == right.ValString)
}
