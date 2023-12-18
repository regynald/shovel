package config

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/indexsupply/x/dig"
	"github.com/indexsupply/x/wos"
	"github.com/indexsupply/x/wpg"
	"github.com/indexsupply/x/wstrings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Root struct {
	Dashboard    Dashboard     `json:"dashboard"`
	PGURL        string        `json:"pg_url"`
	Sources      []Source      `json:"eth_sources"`
	Integrations []Integration `json:"integrations"`
}

func union(a, b wpg.Table) wpg.Table {
	for i := range b.Columns {
		var found bool
		for j := range a.Columns {
			if b.Columns[i].Name == a.Columns[j].Name {
				found = true
				break
			}
		}
		if !found {
			a.Columns = append(a.Columns, wpg.Column{
				Name: b.Columns[i].Name,
				Type: b.Columns[i].Type,
			})
		}
	}
	return a
}

func Migrate(ctx context.Context, pg wpg.Conn, conf Root) error {
	for _, ig := range conf.Integrations {
		if err := ig.Table.Migrate(ctx, pg); err != nil {
			return fmt.Errorf("migrating integration: %s: %w", ig.Name, err)
		}
	}
	return nil
}

func DDL(conf Root) []string {
	var tables = map[string]wpg.Table{}
	for i := range conf.Integrations {
		nt := conf.Integrations[i].Table
		et, exists := tables[nt.Name]
		if exists {
			nt = union(nt, et)
		}
		tables[nt.Name] = nt
	}
	var res []string
	for _, t := range tables {
		for _, stmt := range t.DDL() {
			res = append(res, stmt)
		}
	}
	return res
}

func ValidateFix(conf *Root) error {
	if err := CheckUserInput(*conf); err != nil {
		return fmt.Errorf("checking config for dangerous strings: %w", err)
	}
	for i := range conf.Integrations {
		conf.Integrations[i].AddRequiredFields()
		AddUniqueIndex(&conf.Integrations[i].Table)
		if err := ValidateColRefs(conf.Integrations[i]); err != nil {
			return fmt.Errorf("checking config for references: %w", err)
		}
	}
	return nil
}

func ValidateColRefs(ig Integration) error {
	var (
		ucols   = map[string]struct{}{}
		uinputs = map[string]struct{}{}
		ubd     = map[string]struct{}{}
	)
	for _, c := range ig.Table.Columns {
		if _, ok := ucols[c.Name]; ok {
			return fmt.Errorf("duplicate column: %s", c.Name)
		}
		ucols[c.Name] = struct{}{}
	}
	for _, inp := range ig.Event.Inputs {
		if _, ok := uinputs[inp.Name]; ok {
			return fmt.Errorf("duplicate input: %s", inp.Name)
		}
		uinputs[inp.Name] = struct{}{}
	}
	for _, bd := range ig.Block {
		if _, ok := ubd[bd.Name]; ok {
			return fmt.Errorf("duplicate block data field: %s", bd.Name)
		}
		ubd[bd.Name] = struct{}{}
	}
	// Every selected input must have a coresponding column
	for _, inp := range ig.Event.Selected() {
		var found bool
		for _, c := range ig.Table.Columns {
			if c.Name == inp.Column {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("missing column for %s", inp.Name)
		}
	}
	// Every selected block field must have a coresponding column
	for _, bd := range ig.Block {
		if len(bd.Column) == 0 {
			return fmt.Errorf("missing column for block.%s", bd.Name)
		}
		var found bool
		for _, c := range ig.Table.Columns {
			if c.Name == bd.Column {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("missing column for block.%s", bd.Name)
		}
	}
	return nil
}

// sets default unique columns unless already set by user
func AddUniqueIndex(table *wpg.Table) {
	if len(table.Unique) > 0 {
		return
	}
	possible := []string{
		"ig_name",
		"src_name",
		"block_num",
		"tx_idx",
		"log_idx",
		"abi_idx",
	}
	var uidx []string
	for i := range possible {
		var found bool
		for j := range table.Columns {
			if table.Columns[j].Name == possible[i] {
				found = true
				break
			}
		}
		if found {
			uidx = append(uidx, possible[i])
		}
	}
	if len(uidx) > 0 {
		table.Unique = append(table.Unique, uidx)
	}
}

func CheckUserInput(conf Root) error {
	var (
		err   error
		check = func(name, val string) {
			if err != nil {
				return
			}
			err = wstrings.Safe(val)
			if err != nil {
				err = fmt.Errorf("%q %w", val, err)
			}
		}
	)
	for _, ig := range conf.Integrations {
		check("integration name", ig.Name)
		check("table name", ig.Table.Name)
		for _, c := range ig.Table.Columns {
			check("column name", c.Name)
			check("column type", c.Type)
		}
	}
	for _, sc := range conf.Sources {
		check("source name", sc.Name)
	}
	return err
}

type Dashboard struct {
	EnableLoopbackAuthn bool          `json:"enable_loopback_authn"`
	DisableAuthn        bool          `json:"disable_authn"`
	RootPassword        wos.EnvString `json:"root_password"`
}

type Source struct {
	Name        string        `json:"name"`
	ChainID     uint64        `json:"chain_id"`
	URL         wos.EnvString `json:"url"`
	Start       uint64        `json:"start"`
	Stop        uint64        `json:"stop"`
	Concurrency int           `json:"concurrency"`
	BatchSize   int           `json:"batch_size"`
}

func Sources(ctx context.Context, pgp *pgxpool.Pool) ([]Source, error) {
	var res []Source
	const q = `select name, chain_id, url from shovel.sources`
	rows, err := pgp.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying sources: %w", err)
	}
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.Name, &s.ChainID, &s.URL); err != nil {
			return nil, fmt.Errorf("scanning source: %w", err)
		}
		res = append(res, s)
	}
	return res, nil
}

type Compiled struct {
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
}

type Integration struct {
	Name     string          `json:"name"`
	Enabled  bool            `json:"enabled"`
	Sources  []Source        `json:"sources"`
	Table    wpg.Table       `json:"table"`
	Compiled Compiled        `json:"compiled"`
	Block    []dig.BlockData `json:"block"`
	Event    dig.Event       `json:"event"`
}

func (ig *Integration) AddRequiredFields() {
	hasBD := func(name string) bool {
		for _, bd := range ig.Block {
			if bd.Name == name {
				return true
			}
		}
		return false
	}
	hasCol := func(name string) bool {
		for _, c := range ig.Table.Columns {
			if c.Name == name {
				return true
			}
		}
		return false
	}
	add := func(name, t string) {
		if !hasBD(name) {
			ig.Block = append(ig.Block, dig.BlockData{Name: name, Column: name})
		}
		if !hasCol(name) {
			ig.Table.Columns = append(ig.Table.Columns, wpg.Column{
				Name: name,
				Type: t,
			})
		}
	}
	add("ig_name", "text")
	add("src_name", "text")
	add("block_num", "numeric")
	add("tx_idx", "int")
	if len(ig.Event.Selected()) > 0 {
		add("log_idx", "int")
	}
	for _, inp := range ig.Event.Selected() {
		if !inp.Indexed {
			add("abi_idx", "int2")
		}
	}
}

func Integrations(ctx context.Context, pg wpg.Conn) ([]Integration, error) {
	var res []Integration
	const q = `select conf from shovel.integrations`
	rows, err := pg.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying integrations: %w", err)
	}
	for rows.Next() {
		var buf = []byte{}
		if err := rows.Scan(&buf); err != nil {
			return nil, fmt.Errorf("scanning integration: %w", err)
		}
		var ig Integration
		if err := json.Unmarshal(buf, &ig); err != nil {
			return nil, fmt.Errorf("unmarshaling integration: %w", err)
		}
		res = append(res, ig)
	}
	return res, nil
}

func (ig Integration) Source(name string) (Source, error) {
	for _, sc := range ig.Sources {
		if sc.Name == name {
			return sc, nil
		}
	}
	return Source{}, fmt.Errorf("missing source config for: %s", name)
}

func (conf Root) IntegrationsBySource(ctx context.Context, pg wpg.Conn) (map[string][]Integration, error) {
	indb, err := Integrations(ctx, pg)
	if err != nil {
		return nil, fmt.Errorf("loading db integrations: %w", err)
	}

	var uniq = map[string]Integration{}
	for _, ig := range indb {
		uniq[ig.Name] = ig
	}
	for _, ig := range conf.Integrations {
		uniq[ig.Name] = ig
	}
	res := make(map[string][]Integration)
	for _, ig := range uniq {
		for _, src := range ig.Sources {
			res[src.Name] = append(res[src.Name], ig)
		}
	}
	return res, nil
}

func (conf Root) AllSources(ctx context.Context, pgp *pgxpool.Pool) ([]Source, error) {
	indb, err := Sources(ctx, pgp)
	if err != nil {
		return nil, fmt.Errorf("loading db integrations: %w", err)
	}

	var uniq = map[uint64]Source{}
	for _, src := range indb {
		uniq[src.ChainID] = src
	}
	for _, src := range conf.Sources {
		uniq[src.ChainID] = src
	}

	var res []Source
	for _, src := range uniq {
		res = append(res, src)
	}
	slices.SortFunc(res, func(a, b Source) int {
		return cmp.Compare(a.ChainID, b.ChainID)
	})
	return res, nil
}
