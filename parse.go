// Copyright (c) 2016, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package sh

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Parse reads and parses a shell program with an optional name. It
// returns the parsed program if no issues were encountered. Otherwise,
// an error is returned.
func Parse(r io.Reader, name string) (File, error) {
	p := &parser{
		br: bufio.NewReader(r),
		file: File{
			Name: name,
		},
		npos: Pos{
			Line:   1,
			Column: 1,
		},
	}
	p.next()
	p.stmts(&p.file.Stmts)
	return p.file, p.err
}

type parser struct {
	br *bufio.Reader

	file File
	err  error

	spaced, newLine bool

	ltok, tok Token
	lval, val string

	lpos, pos, npos Pos

	// stack of stop tokens
	stops []Token

	stopNewline bool
	heredocs    []*Word
}

func (p *parser) enterStops(stops ...Token) {
	p.stops = append(p.stops, stops...)
	p.next()
}

func (p *parser) quoted(tok Token) bool {
	return len(p.stops) > 0 && p.stops[len(p.stops)-1] == tok
}
func (p *parser) popStops(n int) { p.stops = p.stops[:len(p.stops)-n] }
func (p *parser) popStop()       { p.popStops(1) }

func (p *parser) readByte() (byte, error) {
	b, err := p.br.ReadByte()
	if err != nil {
		p.errPass(err)
		return 0, err
	}
	p.moveWith(b)
	return b, nil
}
func (p *parser) consumeByte() { p.readByte() }

func (p *parser) moveWith(b byte) {
	if b == '\n' {
		p.npos.Line++
		p.npos.Column = 1
	} else {
		p.npos.Column++
	}
}

func (p *parser) peekByte() (byte, error) {
	bs, err := p.br.Peek(1)
	if err != nil {
		return 0, err
	}
	return bs[0], nil
}

func (p *parser) peekString(s string) bool {
	bs, err := p.br.Peek(len(s))
	return err == nil && string(bs) == s
}

func (p *parser) peekAnyByte(bs ...byte) bool {
	peek, err := p.br.Peek(1)
	if err != nil {
		return false
	}
	return bytes.IndexByte(bs, peek[0]) >= 0
}

func (p *parser) readOnly(s string) bool {
	if p.peekString(s) {
		for i := 0; i < len(s); i++ {
			p.consumeByte()
		}
		return true
	}
	return false
}

var (
	// bytes that form or start a token
	reserved = map[byte]bool{
		'&':  true,
		'>':  true,
		'<':  true,
		'|':  true,
		';':  true,
		'(':  true,
		')':  true,
		'$':  true,
		'"':  true,
		'\'': true,
		'`':  true,
	}
	// subset of the above that mark the end of a word
	wordBreak = map[byte]bool{
		'&': true,
		'>': true,
		'<': true,
		'|': true,
		';': true,
		'(': true,
		')': true,
	}
	// tokenize these inside parameter expansions
	paramOps = map[byte]bool{
		'}': true,
		'#': true,
		':': true,
		'-': true,
		'+': true,
		'=': true,
		'?': true,
		'%': true,
	}
	// tokenize these inside arithmetic expansions
	arithmOps = map[byte]bool{
		'+': true,
		'-': true,
		'!': true,
		'*': true,
		'/': true,
		'%': true,
		'^': true,
	}
	// bytes that will be treated as space
	space = map[byte]bool{
		' ':  true,
		'\t': true,
		'\n': true,
	}
)

func (p *parser) next() {
	if p.tok == EOF {
		return
	}
	p.lpos = p.pos
	p.pos = p.npos
	p.spaced = false
	p.newLine = false
	var b byte
	for {
		if p.readOnly("\\\n") {
			continue
		}
		var err error
		if b, err = p.peekByte(); err != nil {
			p.errPass(err)
			return
		}
		if p.stopNewline && b == '\n' {
			p.advanceTok(STOPPED)
			return
		}
		if p.quoted('"') || !space[b] {
			break
		}
		p.consumeByte()
		p.pos = p.npos
		p.spaced = true
		if b == '\n' {
			p.newLine = true
			if len(p.heredocs) > 0 {
				p.doHeredocs()
				return
			}
		}
	}
	switch {
	case p.quoted(RBRACE) && b == '}', p.quoted(LBRACE) && paramOps[b]:
		if p.readOnly("}") {
			// '}' is a token only in this context
			p.advanceTok(RBRACE)
		} else {
			p.advanceTok(p.doToken(b))
		}
	case b == '#' && !p.quoted('"'):
		p.advanceBoth(COMMENT, p.readLine())
	case p.quoted(DRPAREN) && arithmOps[b]:
		p.advanceTok(p.doToken(b))
	case reserved[b]:
		// Between double quotes, only under certain
		// circumstnaces do we tokenize
		if p.quoted('"') {
			switch {
			case b == '`', b == '"', b == '$', p.tok == DOLLAR:
			default:
				p.advanceReadLit()
				return
			}
		}
		p.advanceTok(p.doToken(b))
	default:
		p.advanceReadLit()
	}
}

func (p *parser) advanceReadLit() { p.advanceBoth(LIT, string(p.readLitBytes())) }
func (p *parser) readLitBytes() (bs []byte) {
	if p.quoted(DRPAREN) {
		p.spaced = true
	}
	for {
		if p.readOnly("\\") { // escaped byte
			if b, _ := p.readByte(); b != '\n' {
				bs = append(bs, '\\', b)
			}
			continue
		}
		b, err := p.peekByte()
		if err != nil {
			return
		}
		switch {
		case b == '$', b == '`':
			return
		case p.quoted(RBRACE) && b == '}':
			return
		case p.quoted(LBRACE) && paramOps[b]:
			return
		case p.quoted('"'):
			if b == '"' {
				return
			}
		case p.quoted(DRPAREN) && arithmOps[b]:
			return
		case reserved[b], space[b]:
			return
		}
		p.consumeByte()
		bs = append(bs, b)
	}
}

func (p *parser) advanceTok(tok Token) { p.advanceBoth(tok, tok.String()) }
func (p *parser) advanceBoth(tok Token, val string) {
	if p.tok != EOF {
		p.ltok = p.tok
		p.lval = p.val
	}
	p.tok = tok
	p.val = val
}

func (p *parser) readUntil(s string) (string, bool) {
	var bs []byte
	for {
		if p.peekString(s) {
			return string(bs), true
		}
		b, err := p.readByte()
		if err != nil {
			return string(bs), false
		}
		bs = append(bs, b)
	}
}

func (p *parser) readLine() string {
	s, _ := p.readUntil("\n")
	return s
}

func (p *parser) doHeredocs() {
	for i, w := range p.heredocs {
		endLine := unquote(*w).String()
		if i > 0 {
			p.readOnly("\n")
		}
		s, _ := p.readHeredocContent(endLine)
		w.Parts[0] = Lit{
			ValuePos: w.Pos(),
			Value:    fmt.Sprintf("%s\n%s", w, s),
		}
		w.Parts = w.Parts[:1]
	}
	p.heredocs = nil
	p.next()
}

func (p *parser) readHeredocContent(endLine string) (string, bool) {
	var buf bytes.Buffer
	for !p.eof() {
		line := p.readLine()
		if line == endLine {
			fmt.Fprint(&buf, line)
			return buf.String(), true
		}
		fmt.Fprintln(&buf, line)
		p.readOnly("\n")
	}
	fmt.Fprint(&buf, endLine)
	return buf.String(), false
}

func (p *parser) peek(tok Token) bool {
	for p.tok == COMMENT {
		p.next()
	}
	return p.tok == tok || p.peekReservedWord(tok)
}

func (p *parser) peekReservedWord(tok Token) bool {
	return p.val == tokNames[tok] && p.peekSpaced()
}

func (p *parser) peekSpaced() bool {
	b, err := p.peekByte()
	return err != nil || space[b] || wordBreak[b]
}

func (p *parser) eof() bool {
	p.peek(COMMENT)
	return p.tok == EOF
}

func (p *parser) peekAny(toks ...Token) bool {
	for _, tok := range toks {
		if p.peek(tok) {
			return true
		}
	}
	return false
}

func (p *parser) got(tok Token) bool {
	if p.peek(tok) {
		p.next()
		return true
	}
	return false
}
func (p *parser) gotSameLine(tok Token) bool { return !p.newLine && p.got(tok) }

func readableStr(v interface{}) string {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case Token:
		s = x.String()
	}
	// don't quote tokens like & or }
	if s[0] >= 'a' && s[0] <= 'z' {
		return strconv.Quote(s)
	}
	return s
}

func (p *parser) followErr(lpos Pos, left interface{}, right string) {
	leftStr := readableStr(left)
	p.posErr(lpos, "%s must be followed by %s", leftStr, right)
}

func (p *parser) wantFollow(lpos Pos, left string, tok Token) {
	if !p.got(tok) {
		p.followErr(lpos, left, fmt.Sprintf(`%q`, tok))
	}
}

func (p *parser) wantFollowStmt(lpos Pos, left string, s *Stmt) {
	if !p.gotStmt(s, false) {
		p.followErr(lpos, left, "a statement")
	}
}

func (p *parser) wantFollowStmts(left Token, sts *[]Stmt, stops ...Token) {
	if p.gotSameLine(SEMICOLON) {
		return
	}
	p.stmts(sts, stops...)
	if len(*sts) < 1 && !p.newLine {
		p.followErr(p.lpos, left, "a statement list")
	}
}

func (p *parser) wantFollowWord(left Token, w *Word) {
	if !p.gotWord(w) {
		p.followErr(p.lpos, left, "a word")
	}
}

func (p *parser) wantStmtEnd(startPos Pos, startTok, tok Token, pos *Pos) {
	if !p.got(tok) {
		p.posErr(startPos, `%s statement must end with %q`, startTok, tok)
	}
	*pos = p.lpos
}

func (p *parser) wantQuote(lpos Pos, b byte) {
	tok := Token(b)
	if !p.got(tok) {
		p.posErr(lpos, `reached %s without closing quote %s`, p.tok, tok)
	}
}

func (p *parser) matchingErr(lpos Pos, left, right Token) {
	p.posErr(lpos, `reached %s without matching token %s with %s`,
		p.tok, left, right)
}

func (p *parser) wantMatched(lpos Pos, left, right Token, rpos *Pos) {
	if !p.got(right) {
		p.matchingErr(lpos, left, right)
	}
	*rpos = p.lpos
}

func (p *parser) errPass(err error) {
	if p.err == nil && err != io.EOF {
		p.err = err
	}
	p.advanceTok(EOF)
}

type lineErr struct {
	pos  Position
	text string
}

func (e lineErr) Error() string {
	return fmt.Sprintf("%s: %s", e.pos, e.text)
}

func (p *parser) posErr(pos Pos, format string, v ...interface{}) {
	p.errPass(lineErr{
		pos: Position{
			Filename: p.file.Name,
			Line:     pos.Line,
			Column:   pos.Column,
		},
		text: fmt.Sprintf(format, v...),
	})
}

func (p *parser) curErr(format string, v ...interface{}) {
	p.posErr(p.pos, format, v...)
}

func (p *parser) stmts(sts *[]Stmt, stops ...Token) {
	for !p.eof() && !p.peekAny(stops...) {
		var s Stmt
		if !p.gotStmt(&s, true, stops...) {
			p.invalidStmtStart()
		}
		*sts = append(*sts, s)
	}
}

func (p *parser) invalidStmtStart() {
	switch {
	case p.peekAny(SEMICOLON, AND, OR, LAND, LOR):
		p.curErr("%s can only immediately follow a statement", p.tok)
	case p.peek(RBRACE):
		p.curErr("%s can only be used to close a block", p.val)
	case p.peek(RPAREN):
		p.curErr("%s can only be used to close a subshell", p.tok)
	default:
		p.curErr("%s is not a valid start for a statement", p.tok)
	}
}

func (p *parser) stmtsNested(sts *[]Stmt, stops ...Token) {
	p.enterStops(stops...)
	p.stmts(sts, stops...)
	p.popStops(len(stops))
}

func (p *parser) gotWord(w *Word) bool {
	p.readParts(&w.Parts)
	return len(w.Parts) > 0
}

func (p *parser) gotLit(l *Lit) bool {
	l.ValuePos = p.pos
	if p.got(LIT) {
		l.Value = p.lval
		return true
	}
	return false
}

func (p *parser) readParts(ns *[]Node) {
	for {
		n := p.wordPart()
		if n == nil {
			break
		}
		*ns = append(*ns, n)
		if p.spaced {
			break
		}
	}
}

func (p *parser) wordPart() Node {
	switch {
	case p.peek(DOLLAR):
		switch {
		case p.peekAnyByte('('):
			// otherwise it is seen as a word break
		case p.peekAnyByte('\'', '"', '`'), p.peekSpaced():
			p.next()
			return Lit{
				ValuePos: p.lpos,
				Value:    p.lval,
			}
		}
		return p.dollar()
	case p.got(LIT):
		return Lit{
			ValuePos: p.lpos,
			Value:    p.lval,
		}
	case p.peek('\''):
		sq := SglQuoted{Quote: p.pos}
		s, found := p.readUntil("'")
		if !found {
			p.wantQuote(sq.Quote, '\'')
		}
		sq.Value = s
		p.readOnly("'")
		p.next()
		return sq
	case !p.quoted('"') && p.peek('"'):
		dq := DblQuoted{Quote: p.pos}
		p.enterStops('"')
		p.readParts(&dq.Parts)
		p.popStop()
		p.wantQuote(dq.Quote, '"')
		return dq
	case !p.quoted('`') && p.peek('`'):
		cs := CmdSubst{Backquotes: true, Left: p.pos}
		p.stmtsNested(&cs.Stmts, '`')
		p.wantQuote(cs.Left, '`')
		cs.Right = p.lpos
		return cs
	}
	return nil
}

func (p *parser) dollar() Node {
	dpos := p.pos
	if p.peekAnyByte('{') {
		return p.paramExp(dpos)
	}
	if p.readOnly("#") {
		p.advanceTok(Token('#'))
	} else {
		p.next()
	}
	lpos := p.pos
	switch {
	case p.peek(LPAREN) && p.readOnly("("):
		p.enterStops(DRPAREN)
		ar := ArithmExpr{
			Dollar: dpos,
			X:      p.arithmExpr(DLPAREN),
		}
		if !p.peekArithmEnd() {
			p.matchingErr(lpos, DLPAREN, DRPAREN)
		}
		ar.Rparen = p.pos
		p.readOnly(")")
		p.popStop()
		p.next()
		return ar
	case p.peek(LPAREN):
		cs := CmdSubst{Left: dpos}
		p.stmtsNested(&cs.Stmts, RPAREN)
		p.wantMatched(lpos, LPAREN, RPAREN, &cs.Right)
		return cs
	default:
		p.next()
		return ParamExp{
			Dollar: dpos,
			Short:  true,
			Param: Lit{
				ValuePos: p.lpos,
				Value:    p.lval,
			},
		}
	}
}

func (p *parser) arithmExpr(following Token) Node {
	if p.eof() || p.peekArithmEnd() {
		return nil
	}
	var left Node
	if p.got(LPAREN) {
		pe := ParenExpr{Lparen: p.lpos}
		pe.X = p.arithmExpr(LPAREN)
		if pe.X == nil {
			p.posErr(pe.Lparen, "parentheses must enclose an expression")
		}
		p.wantMatched(pe.Lparen, LPAREN, RPAREN, &pe.Rparen)
		left = pe
	} else if p.got(ADD) || p.got(SUB) {
		ue := UnaryExpr{
			OpPos: p.lpos,
			Op:    p.ltok,
		}
		ue.X = p.arithmExpr(ue.Op)
		if ue.X == nil {
			p.followErr(ue.OpPos, ue.Op, "an expression")
		}
		left = ue
	} else {
		var w Word
		p.wantFollowWord(following, &w)
		left = w
	}
	if p.got(INC) || p.got(DEC) {
		left = UnaryExpr{
			Post:  true,
			OpPos: p.lpos,
			Op:    p.ltok,
			X:     left,
		}
	}
	if p.eof() || p.peek(RPAREN) {
		return left
	}
	b := BinaryExpr{
		OpPos: p.pos,
		Op:    p.tok,
		X:     left,
	}
	p.next()
	b.Y = p.arithmExpr(b.Op)
	return b
}

func (p *parser) gotParamLit(l *Lit) bool {
	if p.gotLit(l) {
		return true
	}
	switch {
	case p.got(DOLLAR), p.got(QUEST):
		l.Value = p.lval
	default:
		return false
	}
	return true
}

func (p *parser) paramExp(dpos Pos) (pe ParamExp) {
	pe.Dollar = dpos
	lpos := p.npos
	p.readOnly("{")
	p.enterStops(LBRACE)
	pe.Length = p.got(HASH)
	if !p.gotParamLit(&pe.Param) && !pe.Length {
		p.posErr(pe.Dollar, "parameter expansion requires a literal")
	}
	if p.peek(RBRACE) {
		p.popStop()
		p.next()
		return
	}
	if pe.Length {
		p.posErr(pe.Dollar, `string lengths must be like "${#foo}"`)
	}
	pe.Exp = &Expansion{Op: p.tok}
	p.popStop()
	p.enterStops(RBRACE)
	p.gotWord(&pe.Exp.Word)
	p.popStop()
	if !p.got(RBRACE) {
		p.matchingErr(lpos, LBRACE, RBRACE)
	}
	return
}

func (p *parser) peekArithmEnd() bool {
	return p.peek(RPAREN) && p.peekAnyByte(')')
}

func (p *parser) wordList(ws *[]Word) {
	for !p.peekEnd() {
		var w Word
		if !p.gotWord(&w) {
			p.curErr("word list can only contain words")
		}
		*ws = append(*ws, w)
	}
	p.gotSameLine(SEMICOLON)
}

func (p *parser) peekEnd() bool {
	return p.eof() || p.newLine || p.peek(SEMICOLON)
}

func (p *parser) peekStop() bool {
	if p.peekEnd() || p.peekAny(AND, OR, LAND, LOR) {
		return true
	}
	for i := len(p.stops) - 1; i >= 0; i-- {
		stop := p.stops[i]
		if p.peek(stop) {
			return true
		}
		if stop == '`' || stop == RBRACE {
			break
		}
	}
	return false
}

func (p *parser) peekRedir() bool {
	if p.peek(LIT) && p.peekAnyByte('>', '<') {
		return true
	}
	return p.peekAny(RDROUT, APPEND, RDRIN, DPLIN, DPLOUT, RDRINOUT,
		HEREDOC, DHEREDOC, WHEREDOC)
}

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (p *parser) assignSplit() int {
	if !p.peek(LIT) {
		return -1
	}
	i := strings.IndexByte(p.val, '=')
	if i == -1 || !identRe.MatchString(p.val[:i]) {
		return -1
	}
	return i
}

func (p *parser) gotStmt(s *Stmt, wantStop bool, stops ...Token) bool {
	if p.peek(RBRACE) {
		// don't let it be a LIT
		return false
	}
	s.Position = p.pos
	if p.got(NOT) {
		s.Negated = true
	}
	addRedir := func() {
		s.Redirs = append(s.Redirs, p.redirect())
	}
	for {
		if i := p.assignSplit(); i >= 0 {
			name := Lit{ValuePos: p.pos, Value: p.val[:i]}
			start := Lit{
				ValuePos: p.pos,
				Value:    p.val[i+1:],
			}
			var w Word
			if start.Value != "" {
				start.ValuePos.Column += len(name.Value)
				w.Parts = append(w.Parts, start)
			}
			p.next()
			if !p.spaced {
				p.gotWord(&w)
			}
			s.Assigns = append(s.Assigns, Assign{
				Name:  name,
				Value: w,
			})
		} else if p.peekRedir() {
			addRedir()
		} else {
			break
		}
		if p.newLine {
			return true
		}
	}
	p.gotStmtAndOr(s, addRedir)
	if !p.newLine {
		for p.peekRedir() {
			addRedir()
		}
	}
	if !s.Negated && s.Node == nil && len(s.Assigns) == 0 && len(s.Redirs) == 0 {
		return false
	}
	if _, ok := s.Node.(FuncDecl); ok {
		return true
	}
	if wantStop && !p.peekAny(stops...) && !p.peekStop() {
		p.curErr("statements must be separated by &, ; or a newline")
	}
	switch {
	case p.got(LAND), p.got(LOR):
		*s = p.binaryStmt(*s, addRedir)
		return true
	case p.got(AND):
		s.Background = true
	}
	p.gotSameLine(SEMICOLON)
	return true
}

func (p *parser) gotStmtAndOr(s *Stmt, addRedir func()) bool {
	s.Position = p.pos
	switch {
	case p.peek(LPAREN):
		s.Node = p.subshell()
	case p.got(LBRACE):
		s.Node = p.block()
	case p.got(IF):
		s.Node = p.ifStmt()
	case p.got(WHILE):
		s.Node = p.whileStmt()
	case p.got(UNTIL):
		s.Node = p.untilStmt()
	case p.got(FOR):
		s.Node = p.forStmt()
	case p.got(CASE):
		s.Node = p.caseStmt()
	case p.peekAny(LIT, DOLLAR, '"', '\'', '`'):
		s.Node = p.cmdOrFunc(addRedir)
	default:
		return false
	}
	if p.got(OR) {
		*s = p.binaryStmt(*s, addRedir)
	}
	return true
}

func (p *parser) binaryStmt(left Stmt, addRedir func()) Stmt {
	b := BinaryExpr{
		OpPos: p.lpos,
		Op:    p.ltok,
		X:     left,
	}
	var s Stmt
	if b.Op == LAND || b.Op == LOR {
		p.wantFollowStmt(b.OpPos, b.Op.String(), &s)
	} else if !p.gotStmtAndOr(&s, addRedir) {
		p.followErr(b.OpPos, b.Op, "a statement")
	}
	b.Y = s
	return Stmt{
		Position: left.Position,
		Node:     b,
	}
}

func unquote(w Word) (unq Word) {
	for _, n := range w.Parts {
		switch x := n.(type) {
		case SglQuoted:
			unq.Parts = append(unq.Parts, Lit{Value: x.Value})
		case DblQuoted:
			unq.Parts = append(unq.Parts, x.Parts...)
		default:
			unq.Parts = append(unq.Parts, n)
		}
	}
	return unq
}

func (p *parser) redirect() (r Redirect) {
	p.gotLit(&r.N)
	r.Op = p.tok
	r.OpPos = p.pos
	p.next()
	switch r.Op {
	case HEREDOC, DHEREDOC:
		p.stopNewline = true
		p.wantFollowWord(r.Op, &r.Word)
		p.stopNewline = false
		p.heredocs = append(p.heredocs, &r.Word)
		p.got(STOPPED)
	default:
		p.wantFollowWord(r.Op, &r.Word)
	}
	return
}

func (p *parser) subshell() (s Subshell) {
	s.Lparen = p.pos
	p.stmtsNested(&s.Stmts, RPAREN)
	p.wantMatched(s.Lparen, LPAREN, RPAREN, &s.Rparen)
	return
}

func (p *parser) block() (b Block) {
	b.Lbrace = p.lpos
	p.stmts(&b.Stmts, RBRACE)
	p.wantMatched(b.Lbrace, LBRACE, RBRACE, &b.Rbrace)
	return
}

func (p *parser) ifStmt() (fs IfStmt) {
	fs.If = p.lpos
	p.wantFollowStmts(IF, &fs.Conds, THEN)
	p.wantFollow(fs.If, "if [stmts]", THEN)
	p.wantFollowStmts(THEN, &fs.ThenStmts, FI, ELIF, ELSE)
	for p.got(ELIF) {
		elf := Elif{Elif: p.lpos}
		p.wantFollowStmts(ELIF, &elf.Conds, THEN)
		p.wantFollow(elf.Elif, "elif [stmts]", THEN)
		p.wantFollowStmts(THEN, &elf.ThenStmts, FI, ELIF, ELSE)
		fs.Elifs = append(fs.Elifs, elf)
	}
	if p.got(ELSE) {
		p.wantFollowStmts(ELSE, &fs.ElseStmts, FI)
	}
	p.wantStmtEnd(fs.If, IF, FI, &fs.Fi)
	return
}

func (p *parser) whileStmt() (ws WhileStmt) {
	ws.While = p.lpos
	p.wantFollowStmts(WHILE, &ws.Conds, DO)
	p.wantFollow(ws.While, "while [stmts]", DO)
	p.wantFollowStmts(DO, &ws.DoStmts, DONE)
	p.wantStmtEnd(ws.While, WHILE, DONE, &ws.Done)
	return
}

func (p *parser) untilStmt() (us UntilStmt) {
	us.Until = p.lpos
	p.wantFollowStmts(UNTIL, &us.Conds, DO)
	p.wantFollow(us.Until, "until [stmts]", DO)
	p.wantFollowStmts(DO, &us.DoStmts, DONE)
	p.wantStmtEnd(us.Until, UNTIL, DONE, &us.Done)
	return
}

func (p *parser) forStmt() (fs ForStmt) {
	fs.For = p.lpos
	if !p.gotLit(&fs.Name) {
		p.followErr(fs.For, FOR, "a literal")
	}
	if p.got(IN) {
		p.wordList(&fs.WordList)
	} else if !p.gotSameLine(SEMICOLON) && !p.newLine {
		p.followErr(fs.For, "for foo", `"in", ; or a newline`)
	}
	p.wantFollow(fs.For, "for foo [in words]", DO)
	p.wantFollowStmts(DO, &fs.DoStmts, DONE)
	p.wantStmtEnd(fs.For, FOR, DONE, &fs.Done)
	return
}

func (p *parser) caseStmt() (cs CaseStmt) {
	cs.Case = p.lpos
	p.wantFollowWord(CASE, &cs.Word)
	p.wantFollow(cs.Case, "case x", IN)
	cs.List = p.patLists()
	p.wantStmtEnd(cs.Case, CASE, ESAC, &cs.Esac)
	return
}

func (p *parser) patLists() (pls []PatternList) {
	if p.gotSameLine(SEMICOLON) {
		return
	}
	for !p.eof() && !p.peek(ESAC) {
		var pl PatternList
		p.got(LPAREN)
		for !p.eof() {
			var w Word
			if !p.gotWord(&w) {
				p.curErr("case patterns must consist of words")
			}
			pl.Patterns = append(pl.Patterns, w)
			if p.peek(RPAREN) {
				break
			}
			if !p.got(OR) {
				p.curErr("case patterns must be separated with |")
			}
		}
		p.stmtsNested(&pl.Stmts, DSEMICOLON, ESAC)
		pls = append(pls, pl)
		if !p.got(DSEMICOLON) {
			break
		}
	}
	return
}

func (p *parser) cmdOrFunc(addRedir func()) Node {
	if p.got(FUNCTION) {
		fpos := p.lpos
		var w Word
		p.wantFollowWord(FUNCTION, &w)
		if p.gotSameLine(LPAREN) {
			p.wantFollow(w.Pos(), "foo(", RPAREN)
		}
		return p.funcDecl(w, fpos)
	}
	var w Word
	p.gotWord(&w)
	if p.gotSameLine(LPAREN) {
		p.wantFollow(w.Pos(), "foo(", RPAREN)
		return p.funcDecl(w, w.Pos())
	}
	cmd := Command{Args: []Word{w}}
	for !p.peekStop() {
		var w Word
		switch {
		case p.peekRedir():
			addRedir()
		case p.gotWord(&w):
			cmd.Args = append(cmd.Args, w)
		default:
			p.curErr("a command can only contain words and redirects")
		}
	}
	return cmd
}

func (p *parser) funcDecl(w Word, pos Pos) FuncDecl {
	fd := FuncDecl{
		Position:  pos,
		BashStyle: pos != w.Pos(),
		Name: Lit{
			Value:    w.String(),
			ValuePos: w.Pos(),
		},
	}
	if !identRe.MatchString(fd.Name.Value) {
		p.posErr(fd.Pos(), "invalid func name: %s", fd.Name.Value)
	}
	p.wantFollowStmt(fd.Pos(), "foo()", &fd.Body)
	return fd
}
