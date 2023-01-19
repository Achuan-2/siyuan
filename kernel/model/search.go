// SiYuan - Build Your Eternal Digital Garden
// Copyright (c) 2020-present, b3log.org
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package model

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/88250/gulu"
	"github.com/88250/lute"
	"github.com/88250/lute/ast"
	"github.com/88250/lute/lex"
	"github.com/88250/lute/parse"
	"github.com/jinzhu/copier"
	"github.com/siyuan-note/logging"
	"github.com/siyuan-note/siyuan/kernel/conf"
	"github.com/siyuan-note/siyuan/kernel/search"
	"github.com/siyuan-note/siyuan/kernel/sql"
	"github.com/siyuan-note/siyuan/kernel/task"
	"github.com/siyuan-note/siyuan/kernel/treenode"
	"github.com/siyuan-note/siyuan/kernel/util"
	"github.com/xrash/smetrics"
)

type EmbedBlock struct {
	Block      *Block       `json:"block"`
	BlockPaths []*BlockPath `json:"blockPaths"`
}

func SearchEmbedBlock(embedBlockID, stmt string, excludeIDs []string, headingMode int, breadcrumb bool) (ret []*EmbedBlock) {
	time.Sleep(util.FrontendQueueInterval)
	WaitForWritingFiles()
	return searchEmbedBlock(embedBlockID, stmt, excludeIDs, headingMode, breadcrumb)
}

func searchEmbedBlock(embedBlockID, stmt string, excludeIDs []string, headingMode int, breadcrumb bool) (ret []*EmbedBlock) {
	sqlBlocks := sql.SelectBlocksRawStmtNoParse(stmt, Conf.Search.Limit)
	var tmp []*sql.Block
	for _, b := range sqlBlocks {
		if "query_embed" == b.Type { // 嵌入块不再嵌入
			// 嵌入块支持搜索 https://github.com/siyuan-note/siyuan/issues/7112
			// 这里会导致上面的 limit 限制不准确，导致结果变少，暂时没有解决方案，只能靠用户自己调整 SQL，加上 type != 'query_embed' 的条件
			continue
		}
		if !gulu.Str.Contains(b.ID, excludeIDs) {
			tmp = append(tmp, b)
		}
	}
	sqlBlocks = tmp

	// 嵌入块支持搜索 https://github.com/siyuan-note/siyuan/issues/7112
	task.AppendTask(task.DatabaseIndexEmbedBlock, updateEmbedBlockContent, embedBlockID, sqlBlocks)

	// 缓存最多 128 棵语法树
	trees := map[string]*parse.Tree{}
	count := 0
	for _, sb := range sqlBlocks {
		if nil == trees[sb.RootID] {
			tree, _ := loadTreeByBlockID(sb.RootID)
			if nil == tree {
				continue
			}
			trees[sb.RootID] = tree
			count++
		}
		if 127 < count {
			break
		}
	}

	for _, sb := range sqlBlocks {
		block, blockPaths := getEmbeddedBlock(embedBlockID, trees, sb, headingMode, breadcrumb)
		if nil == block {
			continue
		}
		ret = append(ret, &EmbedBlock{
			Block:      block,
			BlockPaths: blockPaths,
		})
	}

	// 添加笔记本名称
	var boxIDs []string
	for _, embedBlock := range ret {
		boxIDs = append(boxIDs, embedBlock.Block.Box)
	}
	boxIDs = gulu.Str.RemoveDuplicatedElem(boxIDs)
	boxNames := Conf.BoxNames(boxIDs)
	for _, embedBlock := range ret {
		name := boxNames[embedBlock.Block.Box]
		embedBlock.Block.HPath = name + embedBlock.Block.HPath
	}

	if 1 > len(ret) {
		ret = []*EmbedBlock{}
	}
	return
}

func SearchRefBlock(id, rootID, keyword string, beforeLen int) (ret []*Block, newDoc bool) {
	if "" == keyword {
		// 查询为空时默认的块引排序规则按最近使用优先 https://github.com/siyuan-note/siyuan/issues/3218
		refs := sql.QueryRefsRecent()
		for _, ref := range refs {
			sqlBlock := sql.GetBlock(ref.DefBlockID)
			block := fromSQLBlock(sqlBlock, "", beforeLen)
			if nil == block {
				continue
			}
			block.RefText = sql.GetRefText(block.ID)
			block.RefText = maxContent(block.RefText, Conf.Editor.BlockRefDynamicAnchorTextMaxLen)
			ret = append(ret, block)
		}
		if 1 > len(ret) {
			ret = []*Block{}
		}
		return
	}

	ret = fullTextSearchRefBlock(keyword, beforeLen)
	tmp := ret[:0]
	trees := map[string]*parse.Tree{}
	for _, b := range ret {
		hitFirstChildID := false
		b.RefText = sql.GetRefText(b.ID)
		b.RefText = maxContent(b.RefText, Conf.Editor.BlockRefDynamicAnchorTextMaxLen)
		if b.IsContainerBlock() {

			// `((` 引用候选中排除当前块的父块 https://github.com/siyuan-note/siyuan/issues/4538
			tree := trees[b.RootID]
			if nil == tree {
				tree, _ = loadTreeByBlockID(b.RootID)
				trees[b.RootID] = tree
			}
			if nil != tree {
				bNode := treenode.GetNodeInTree(tree, b.ID)
				if fc := treenode.FirstLeafBlock(bNode); nil != fc && fc.ID == id {
					hitFirstChildID = true
				}
			}
		}

		if b.ID != id && !hitFirstChildID && b.ID != rootID {
			tmp = append(tmp, b)
		}
	}
	ret = tmp

	if "" != keyword {
		if block := treenode.GetBlockTree(id); nil != block {
			p := path.Join(block.HPath, keyword)
			newDoc = nil == treenode.GetBlockTreeRootByHPath(block.BoxID, p)
		}
	}
	return
}

func FindReplace(keyword, replacement string, ids []string, method int) (err error) {
	// method：0：文本，1：查询语法，2：SQL，3：正则表达式
	if 1 == method || 2 == method {
		err = errors.New(Conf.Language(132))
		return
	}

	if keyword == replacement {
		return
	}

	ids = gulu.Str.RemoveDuplicatedElem(ids)
	var renameRoots []*ast.Node
	renameRootTitles := map[string]string{}
	for _, id := range ids {
		var tree *parse.Tree
		tree, err = loadTreeByBlockID(id)
		if nil != err {
			return
		}

		node := treenode.GetNodeInTree(tree, id)
		if nil == node {
			return
		}

		ast.Walk(node, func(n *ast.Node, entering bool) ast.WalkStatus {
			if !entering {
				return ast.WalkContinue
			}

			switch n.Type {
			case ast.NodeDocument:
				title := n.IALAttr("title")
				if 0 == method {
					if strings.Contains(title, keyword) {
						renameRootTitles[n.ID] = strings.ReplaceAll(title, keyword, replacement)
						renameRoots = append(renameRoots, n)
					}
				} else if 3 == method {
					r, _ := regexp.Compile(keyword)
					if nil != r && r.MatchString(title) {
						renameRootTitles[n.ID] = r.ReplaceAllString(title, replacement)
						renameRoots = append(renameRoots, n)
					}
				}
			case ast.NodeText, ast.NodeLinkDest, ast.NodeLinkText, ast.NodeLinkTitle, ast.NodeCodeBlockCode, ast.NodeMathBlockContent:
				if 0 == method {
					if bytes.Contains(n.Tokens, []byte(keyword)) {
						n.Tokens = bytes.ReplaceAll(n.Tokens, []byte(keyword), []byte(replacement))
					}
				} else if 3 == method {
					r, _ := regexp.Compile(keyword)
					if nil != r && r.MatchString(string(n.Tokens)) {
						n.Tokens = []byte(r.ReplaceAllString(string(n.Tokens), replacement))
					}
				}
			case ast.NodeTextMark:
				if n.IsTextMarkType("code") {
					escapedKey := util.EscapeHTML(keyword)
					if 0 == method {
						if strings.Contains(n.TextMarkTextContent, escapedKey) {
							n.TextMarkTextContent = strings.ReplaceAll(n.TextMarkTextContent, escapedKey, replacement)
						}
					} else if 3 == method {
						r, _ := regexp.Compile(escapedKey)
						if nil != r && r.MatchString(n.TextMarkTextContent) {
							n.TextMarkTextContent = r.ReplaceAllString(n.TextMarkTextContent, replacement)
						}
					}
				} else {
					if 0 == method {
						if bytes.Contains(n.Tokens, []byte(keyword)) {
							n.TextMarkTextContent = strings.ReplaceAll(n.TextMarkTextContent, keyword, replacement)
						}
					} else if 3 == method {
						r, _ := regexp.Compile(keyword)
						if nil != r && r.MatchString(n.TextMarkTextContent) {
							n.TextMarkTextContent = r.ReplaceAllString(n.TextMarkTextContent, replacement)
						}
					}
				}

				if 0 == method {
					if strings.Contains(n.TextMarkTextContent, keyword) {
						n.TextMarkTextContent = strings.ReplaceAll(n.TextMarkTextContent, keyword, replacement)
					}
					if strings.Contains(n.TextMarkInlineMathContent, keyword) {
						n.TextMarkInlineMathContent = strings.ReplaceAll(n.TextMarkInlineMathContent, keyword, replacement)
					}
					if strings.Contains(n.TextMarkInlineMemoContent, keyword) {
						n.TextMarkInlineMemoContent = strings.ReplaceAll(n.TextMarkInlineMemoContent, keyword, replacement)
					}
					if strings.Contains(n.TextMarkATitle, keyword) {
						n.TextMarkATitle = strings.ReplaceAll(n.TextMarkATitle, keyword, replacement)
					}
					if strings.Contains(n.TextMarkAHref, keyword) {
						n.TextMarkAHref = strings.ReplaceAll(n.TextMarkAHref, keyword, replacement)
					}
				} else if 3 == method {
					r, _ := regexp.Compile(keyword)
					if nil != r {
						if r.MatchString(n.TextMarkTextContent) {
							n.TextMarkTextContent = r.ReplaceAllString(n.TextMarkTextContent, replacement)
						}
						if r.MatchString(n.TextMarkInlineMathContent) {
							n.TextMarkInlineMathContent = r.ReplaceAllString(n.TextMarkInlineMathContent, replacement)
						}
						if r.MatchString(n.TextMarkInlineMemoContent) {
							n.TextMarkInlineMemoContent = r.ReplaceAllString(n.TextMarkInlineMemoContent, replacement)
						}
						if r.MatchString(n.TextMarkATitle) {
							n.TextMarkATitle = r.ReplaceAllString(n.TextMarkATitle, replacement)
						}
						if r.MatchString(n.TextMarkAHref) {
							n.TextMarkAHref = r.ReplaceAllString(n.TextMarkAHref, replacement)
						}
					}
				}
			}
			return ast.WalkContinue
		})

		if err = writeJSONQueue(tree); nil != err {
			return
		}
	}

	for _, renameRoot := range renameRoots {
		newTitle := renameRootTitles[renameRoot.ID]
		RenameDoc(renameRoot.Box, renameRoot.Path, newTitle)
	}

	WaitForWritingFiles()
	if 1 < len(ids) {
		go func() {
			time.Sleep(time.Second)
			util.ReloadUI()
		}()
	}
	return
}

// FullTextSearchBlock 搜索内容块。
//
// method：0：关键字，1：查询语法，2：SQL，3：正则表达式
// orderBy: 0：按块类型（默认），1：按创建时间升序，2：按创建时间降序，3：按更新时间升序，4：按更新时间降序，5：按内容顺序（仅在按文档分组时），6：按相关度升序，7：按相关度降序
// groupBy：0：不分组，1：按文档分组
func FullTextSearchBlock(query string, boxes, paths []string, types map[string]bool, method, orderBy, groupBy int) (ret []*Block, matchedBlockCount, matchedRootCount int) {
	query = strings.TrimSpace(query)
	beforeLen := 36
	var blocks []*Block
	orderByClause := buildOrderBy(orderBy)
	switch method {
	case 1: // 查询语法
		filter := buildTypeFilter(types)
		boxFilter := buildBoxesFilter(boxes)
		pathFilter := buildPathsFilter(paths)
		blocks, matchedBlockCount, matchedRootCount = fullTextSearchByQuerySyntax(query, boxFilter, pathFilter, filter, orderByClause, beforeLen)
	case 2: // SQL
		blocks, matchedBlockCount, matchedRootCount = searchBySQL(query, beforeLen)
	case 3: // 正则表达式
		typeFilter := buildTypeFilter(types)
		boxFilter := buildBoxesFilter(boxes)
		pathFilter := buildPathsFilter(paths)
		blocks, matchedBlockCount, matchedRootCount = fullTextSearchByRegexp(query, boxFilter, pathFilter, typeFilter, orderByClause, beforeLen)
	default: // 关键字
		filter := buildTypeFilter(types)
		boxFilter := buildBoxesFilter(boxes)
		pathFilter := buildPathsFilter(paths)
		blocks, matchedBlockCount, matchedRootCount = fullTextSearchByKeyword(query, boxFilter, pathFilter, filter, orderByClause, beforeLen)
	}

	switch groupBy {
	case 0: // 不分组
		ret = blocks
	case 1: // 按文档分组
		rootMap := map[string]bool{}
		var rootIDs []string
		contentSorts := map[string]int{}
		for _, b := range blocks {
			if _, ok := rootMap[b.RootID]; !ok {
				rootMap[b.RootID] = true
				rootIDs = append(rootIDs, b.RootID)
				tree, _ := loadTreeByBlockID(b.RootID)
				if nil == tree {
					continue
				}

				if 5 == orderBy { // 按内容顺序（仅在按文档分组时）
					sort := 0
					ast.Walk(tree.Root, func(n *ast.Node, entering bool) ast.WalkStatus {
						if !entering || !n.IsBlock() {
							return ast.WalkContinue
						}

						contentSorts[n.ID] = sort
						sort++
						return ast.WalkContinue
					})
				}
			}
		}

		sqlRoots := sql.GetBlocks(rootIDs)
		roots := fromSQLBlocks(&sqlRoots, "", beforeLen)
		for _, root := range roots {
			for _, b := range blocks {
				if 5 == orderBy { // 按内容顺序（仅在按文档分组时）
					b.Sort = contentSorts[b.ID]
				}
				if b.RootID == root.ID {
					root.Children = append(root.Children, b)
				}
			}

			switch orderBy {
			case 1: //按创建时间升序
				sort.Slice(root.Children, func(i, j int) bool { return root.Children[i].Created < root.Children[j].Created })
			case 2: // 按创建时间降序
				sort.Slice(root.Children, func(i, j int) bool { return root.Children[i].Created > root.Children[j].Created })
			case 3: // 按更新时间升序
				sort.Slice(root.Children, func(i, j int) bool { return root.Children[i].Updated < root.Children[j].Updated })
			case 4: // 按更新时间降序
				sort.Slice(root.Children, func(i, j int) bool { return root.Children[i].Updated > root.Children[j].Updated })
			case 5: // 按内容顺序（仅在按文档分组时）
				sort.Slice(root.Children, func(i, j int) bool { return root.Children[i].Sort < root.Children[j].Sort })
			default: // 按块类型（默认）
				sort.Slice(root.Children, func(i, j int) bool { return root.Children[i].Sort < root.Children[j].Sort })
			}
		}

		switch orderBy {
		case 1: //按创建时间升序
			sort.Slice(roots, func(i, j int) bool { return roots[i].Created < roots[j].Created })
		case 2: // 按创建时间降序
			sort.Slice(roots, func(i, j int) bool { return roots[i].Created > roots[j].Created })
		case 3: // 按更新时间升序
			sort.Slice(roots, func(i, j int) bool { return roots[i].Updated < roots[j].Updated })
		case 4: // 按更新时间降序
			sort.Slice(roots, func(i, j int) bool { return roots[i].Updated > roots[j].Updated })
		case 5: // 按内容顺序（仅在按文档分组时）
		// 都是文档，不需要再次排序
		case 6, 7: // 按相关度
		// 已在 ORDER BY 中处理
		default: // 按块类型（默认）
			// 都是文档，不需要再次排序
		}
		ret = roots
	default:
		ret = blocks
	}
	if 1 > len(ret) {
		ret = []*Block{}
	}
	return
}

func buildBoxesFilter(boxes []string) string {
	if 0 == len(boxes) {
		return ""
	}
	builder := bytes.Buffer{}
	builder.WriteString(" AND (")
	for i, box := range boxes {
		builder.WriteString(fmt.Sprintf("box = '%s'", box))
		if i < len(boxes)-1 {
			builder.WriteString(" OR ")
		}
	}
	builder.WriteString(")")
	return builder.String()
}

func buildPathsFilter(paths []string) string {
	if 0 == len(paths) {
		return ""
	}
	builder := bytes.Buffer{}
	builder.WriteString(" AND (")
	for i, path := range paths {
		builder.WriteString(fmt.Sprintf("path LIKE '%s%%'", path))
		if i < len(paths)-1 {
			builder.WriteString(" OR ")
		}
	}
	builder.WriteString(")")
	return builder.String()
}

func buildOrderBy(orderBy int) string {
	switch orderBy {
	case 1:
		return "ORDER BY created ASC"
	case 2:
		return "ORDER BY created DESC"
	case 3:
		return "ORDER BY updated ASC"
	case 4:
		return "ORDER BY updated DESC"
	case 6:
		return "ORDER BY rank DESC" // 默认是按相关度降序，所以按相关度升序要反过来使用 DESC
	case 7:
		return "ORDER BY rank" // 默认是按相关度降序
	default:
		return "ORDER BY sort ASC"
	}
}

func buildTypeFilter(types map[string]bool) string {
	s := conf.NewSearch()
	if err := copier.Copy(s, Conf.Search); nil != err {
		logging.LogErrorf("copy search conf failed: %s", err)
	}
	if nil != types {
		s.Document = types["document"]
		s.Heading = types["heading"]
		s.List = types["list"]
		s.ListItem = types["listItem"]
		s.CodeBlock = types["codeBlock"]
		s.MathBlock = types["mathBlock"]
		s.Table = types["table"]
		s.Blockquote = types["blockquote"]
		s.SuperBlock = types["superBlock"]
		s.Paragraph = types["paragraph"]
		s.HTMLBlock = types["htmlBlock"]
		s.EmbedBlock = types["embedBlock"]
	} else {
		s.Document = Conf.Search.Document
		s.Heading = Conf.Search.Heading
		s.List = Conf.Search.List
		s.ListItem = Conf.Search.ListItem
		s.CodeBlock = Conf.Search.CodeBlock
		s.MathBlock = Conf.Search.MathBlock
		s.Table = Conf.Search.Table
		s.Blockquote = Conf.Search.Blockquote
		s.SuperBlock = Conf.Search.SuperBlock
		s.Paragraph = Conf.Search.Paragraph
		s.HTMLBlock = Conf.Search.HTMLBlock
		s.EmbedBlock = Conf.Search.EmbedBlock
	}
	return s.TypeFilter()
}

func searchBySQL(stmt string, beforeLen int) (ret []*Block, matchedBlockCount, matchedRootCount int) {
	stmt = gulu.Str.RemoveInvisible(stmt)
	blocks := sql.SelectBlocksRawStmt(stmt, Conf.Search.Limit)
	ret = fromSQLBlocks(&blocks, "", beforeLen)
	if 1 > len(ret) {
		ret = []*Block{}
		return
	}

	stmt = strings.ToLower(stmt)
	stmt = strings.ReplaceAll(stmt, "select * ", "select COUNT(id) AS `matches`, COUNT(DISTINCT(root_id)) AS `docs` ")
	result, _ := sql.Query(stmt)
	if 1 > len(ret) {
		return
	}

	matchedBlockCount = int(result[0]["matches"].(int64))
	matchedRootCount = int(result[0]["docs"].(int64))
	return
}

func fullTextSearchRefBlock(keyword string, beforeLen int) (ret []*Block) {
	keyword = gulu.Str.RemoveInvisible(keyword)

	if ast.IsNodeIDPattern(keyword) {
		ret, _, _ = searchBySQL("SELECT * FROM `blocks` WHERE `id` = '"+keyword+"'", 36)
		return
	}

	quotedKeyword := stringQuery(keyword)
	table := "blocks_fts" // 大小写敏感
	if !Conf.Search.CaseSensitive {
		table = "blocks_fts_case_insensitive"
	}

	projections := "id, parent_id, root_id, hash, box, path, " +
		"snippet(" + table + ", 6, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "', '...', 64) AS hpath, " +
		"snippet(" + table + ", 7, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "', '...', 64) AS name, " +
		"snippet(" + table + ", 8, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "', '...', 64) AS alias, " +
		"snippet(" + table + ", 9, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "', '...', 64) AS memo, " +
		"tag, " +
		"snippet(" + table + ", 11, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "', '...', 64) AS content, " +
		"fcontent, markdown, length, type, subtype, ial, sort, created, updated"
	stmt := "SELECT " + projections + " FROM " + table + " WHERE " + table + " MATCH '" + columnFilter() + ":(" + quotedKeyword + ")' AND type IN " + Conf.Search.TypeFilter()
	orderBy := ` order by case
             when name = '${keyword}' then 10
             when alias = '${keyword}' then 20
             when memo = '${keyword}' then 30
             when content = '${keyword}' and type = 'd' then 40
             when content LIKE '%${keyword}%' and type = 'd' then 41
             when name LIKE '%${keyword}%' then 50
             when alias LIKE '%${keyword}%' then 60
             when content = '${keyword}' and type = 'h' then 70
             when content LIKE '%${keyword}%' and type = 'h' then 71
             when fcontent = '${keyword}' and type = 'i' then 80
             when fcontent LIKE '%${keyword}%' and type = 'i' then 81
             when memo LIKE '%${keyword}%' then 90
             when content LIKE '%${keyword}%' and type != 'i' and type != 'l' then 100
             else 65535 end ASC, sort ASC, length ASC`
	orderBy = strings.ReplaceAll(orderBy, "${keyword}", keyword)
	stmt += orderBy + " LIMIT " + strconv.Itoa(Conf.Search.Limit)
	blocks := sql.SelectBlocksRawStmt(stmt, Conf.Search.Limit)
	ret = fromSQLBlocks(&blocks, "", beforeLen)
	if 1 > len(ret) {
		ret = []*Block{}
	}
	return
}

func fullTextSearchByQuerySyntax(query, boxFilter, pathFilter, typeFilter, orderBy string, beforeLen int) (ret []*Block, matchedBlockCount, matchedRootCount int) {
	query = gulu.Str.RemoveInvisible(query)
	if ast.IsNodeIDPattern(query) {
		ret, matchedBlockCount, matchedRootCount = searchBySQL("SELECT * FROM `blocks` WHERE `id` = '"+query+"'", beforeLen)
		return
	}
	return fullTextSearchByFTS(query, boxFilter, pathFilter, typeFilter, orderBy, beforeLen)
}

func fullTextSearchByKeyword(query, boxFilter, pathFilter, typeFilter string, orderBy string, beforeLen int) (ret []*Block, matchedBlockCount, matchedRootCount int) {
	query = gulu.Str.RemoveInvisible(query)
	if ast.IsNodeIDPattern(query) {
		ret, matchedBlockCount, matchedRootCount = searchBySQL("SELECT * FROM `blocks` WHERE `id` = '"+query+"'", beforeLen)
		return
	}
	query = stringQuery(query)
	return fullTextSearchByFTS(query, boxFilter, pathFilter, typeFilter, orderBy, beforeLen)
}

func fullTextSearchByRegexp(exp, boxFilter, pathFilter, typeFilter, orderBy string, beforeLen int) (ret []*Block, matchedBlockCount, matchedRootCount int) {
	exp = gulu.Str.RemoveInvisible(exp)
	exp = regexp.QuoteMeta(exp)

	fieldFilter := fieldRegexp(exp)
	stmt := "SELECT * FROM `blocks` WHERE " + fieldFilter + " AND type IN " + typeFilter
	stmt += boxFilter + pathFilter
	stmt += " " + orderBy
	stmt += " LIMIT " + strconv.Itoa(Conf.Search.Limit)
	blocks := sql.SelectBlocksRawStmt(stmt, Conf.Search.Limit)
	ret = fromSQLBlocks(&blocks, "", beforeLen)
	if 1 > len(ret) {
		ret = []*Block{}
	}

	matchedBlockCount, matchedRootCount = fullTextSearchCountByRegexp(exp, boxFilter, pathFilter, typeFilter)
	return
}

func fullTextSearchCountByRegexp(exp, boxFilter, pathFilter, typeFilter string) (matchedBlockCount, matchedRootCount int) {
	fieldFilter := fieldRegexp(exp)
	stmt := "SELECT COUNT(id) AS `matches`, COUNT(DISTINCT(root_id)) AS `docs` FROM `blocks` WHERE " + fieldFilter + " AND type IN " + typeFilter
	stmt += boxFilter + pathFilter
	stmt += " LIMIT " + strconv.Itoa(Conf.Search.Limit)
	result, _ := sql.Query(stmt)
	if 1 > len(result) {
		return
	}
	matchedBlockCount = int(result[0]["matches"].(int64))
	matchedRootCount = int(result[0]["docs"].(int64))
	return
}

func fullTextSearchByFTS(query, boxFilter, pathFilter, typeFilter, orderBy string, beforeLen int) (ret []*Block, matchedBlockCount, matchedRootCount int) {
	table := "blocks_fts" // 大小写敏感
	if !Conf.Search.CaseSensitive {
		table = "blocks_fts_case_insensitive"
	}
	projections := "id, parent_id, root_id, hash, box, path, " +
		"highlight(" + table + ", 6, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "') AS hpath, " +
		"highlight(" + table + ", 7, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "') AS name, " +
		"highlight(" + table + ", 8, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "') AS alias, " +
		"highlight(" + table + ", 9, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "') AS memo, " +
		"tag, " +
		"highlight(" + table + ", 11, '" + search.SearchMarkLeft + "', '" + search.SearchMarkRight + "') AS content, " +
		"fcontent, markdown, length, type, subtype, ial, sort, created, updated"
	stmt := "SELECT " + projections + " FROM " + table + " WHERE " + table + " MATCH '" + columnFilter() + ":(" + query + ")' AND type IN " + typeFilter
	stmt += boxFilter + pathFilter
	stmt += " " + orderBy
	stmt += " LIMIT " + strconv.Itoa(Conf.Search.Limit)
	blocks := sql.SelectBlocksRawStmt(stmt, Conf.Search.Limit)
	ret = fromSQLBlocks(&blocks, "", beforeLen)
	if 1 > len(ret) {
		ret = []*Block{}
	}

	matchedBlockCount, matchedRootCount = fullTextSearchCount(query, boxFilter, pathFilter, typeFilter)
	return
}

func fullTextSearchCount(query, boxFilter, pathFilter, typeFilter string) (matchedBlockCount, matchedRootCount int) {
	query = gulu.Str.RemoveInvisible(query)
	if ast.IsNodeIDPattern(query) {
		ret, _ := sql.Query("SELECT COUNT(id) AS `matches`, COUNT(DISTINCT(root_id)) AS `docs` FROM `blocks` WHERE `id` = '" + query + "'")
		if 1 > len(ret) {
			return
		}
		matchedBlockCount = int(ret[0]["matches"].(int64))
		matchedRootCount = int(ret[0]["docs"].(int64))
		return
	}

	table := "blocks_fts" // 大小写敏感
	if !Conf.Search.CaseSensitive {
		table = "blocks_fts_case_insensitive"
	}

	stmt := "SELECT COUNT(id) AS `matches`, COUNT(DISTINCT(root_id)) AS `docs` FROM `" + table + "` WHERE `" + table + "` MATCH '" + columnFilter() + ":(" + query + ")' AND type IN " + typeFilter
	stmt += boxFilter + pathFilter
	result, _ := sql.Query(stmt)
	if 1 > len(result) {
		return
	}
	matchedBlockCount = int(result[0]["matches"].(int64))
	matchedRootCount = int(result[0]["docs"].(int64))
	return
}

func markSearch(text string, keyword string, beforeLen int) (marked string, score float64) {
	if 0 == len(keyword) {
		marked = text

		if strings.Contains(marked, search.SearchMarkLeft) { // 使用 FTS snippet() 处理过高亮片段，这里简单替换后就返回
			marked = util.EscapeHTML(text)
			marked = strings.ReplaceAll(marked, search.SearchMarkLeft, "<mark>")
			marked = strings.ReplaceAll(marked, search.SearchMarkRight, "</mark>")
			return
		}

		keywords := gulu.Str.SubstringsBetween(marked, search.SearchMarkLeft, search.SearchMarkRight)
		keywords = gulu.Str.RemoveDuplicatedElem(keywords)
		keyword = strings.Join(keywords, search.TermSep)
		marked = strings.ReplaceAll(marked, search.SearchMarkLeft, "")
		marked = strings.ReplaceAll(marked, search.SearchMarkRight, "")
		_, marked = search.MarkText(marked, keyword, beforeLen, Conf.Search.CaseSensitive)
		return
	}

	pos, marked := search.MarkText(text, keyword, beforeLen, Conf.Search.CaseSensitive)
	if -1 < pos {
		if 0 == pos {
			score = 1
		}
		score += float64(strings.Count(marked, "<mark>"))
		winkler := smetrics.JaroWinkler(text, keyword, 0.7, 4)
		score += winkler
	}
	score = -score // 分越小排序越靠前
	return
}

func fromSQLBlocks(sqlBlocks *[]*sql.Block, terms string, beforeLen int) (ret []*Block) {
	for _, sqlBlock := range *sqlBlocks {
		ret = append(ret, fromSQLBlock(sqlBlock, terms, beforeLen))
	}
	return
}

func fromSQLBlock(sqlBlock *sql.Block, terms string, beforeLen int) (block *Block) {
	if nil == sqlBlock {
		return
	}

	id := sqlBlock.ID
	content := sqlBlock.Content
	p := sqlBlock.Path

	content, _ = markSearch(content, terms, beforeLen)
	content = maxContent(content, 5120)
	markdown := maxContent(sqlBlock.Markdown, 5120)

	block = &Block{
		Box:      sqlBlock.Box,
		Path:     p,
		ID:       id,
		RootID:   sqlBlock.RootID,
		ParentID: sqlBlock.ParentID,
		Alias:    sqlBlock.Alias,
		Name:     sqlBlock.Name,
		Memo:     sqlBlock.Memo,
		Tag:      sqlBlock.Tag,
		Content:  content,
		FContent: sqlBlock.FContent,
		Markdown: markdown,
		Type:     treenode.FromAbbrType(sqlBlock.Type),
		SubType:  sqlBlock.SubType,
		Sort:     sqlBlock.Sort,
	}
	if "" != sqlBlock.IAL {
		block.IAL = map[string]string{}
		ialStr := strings.TrimPrefix(sqlBlock.IAL, "{:")
		ialStr = strings.TrimSuffix(ialStr, "}")
		ial := parse.Tokens2IAL([]byte(ialStr))
		for _, kv := range ial {
			block.IAL[kv[0]] = kv[1]
		}
	}

	hPath, _ := markSearch(sqlBlock.HPath, terms, 18)
	if !strings.HasPrefix(hPath, "/") {
		hPath = "/" + hPath
	}
	block.HPath = hPath

	if "" != block.Name {
		block.Name, _ = markSearch(block.Name, terms, 256)
	}
	if "" != block.Alias {
		block.Alias, _ = markSearch(block.Alias, terms, 256)
	}
	if "" != block.Memo {
		block.Memo, _ = markSearch(block.Memo, terms, 256)
	}
	return
}

func maxContent(content string, maxLen int) string {
	idx := strings.Index(content, "<mark>")
	if 128 < maxLen && maxLen <= idx {
		head := bytes.Buffer{}
		for i := 0; i < 512; i++ {
			r, size := utf8.DecodeLastRuneInString(content[:idx])
			head.WriteRune(r)
			idx -= size
			if 64 < head.Len() {
				break
			}
		}

		content = util.Reverse(head.String()) + content[idx:]
	}

	if maxLen < utf8.RuneCountInString(content) {
		return gulu.Str.SubStr(content, maxLen) + "..."
	}
	return content
}

func fieldRegexp(regexp string) string {
	buf := bytes.Buffer{}
	buf.WriteString("(")
	buf.WriteString("content REGEXP '")
	buf.WriteString(regexp)
	buf.WriteString("'")
	if Conf.Search.Name {
		buf.WriteString(" OR name REGEXP '")
		buf.WriteString(regexp)
		buf.WriteString("'")
	}
	if Conf.Search.Alias {
		buf.WriteString(" OR alias REGEXP '")
		buf.WriteString(regexp)
		buf.WriteString("'")
	}
	if Conf.Search.Memo {
		buf.WriteString(" OR memo REGEXP '")
		buf.WriteString(regexp)
		buf.WriteString("'")
	}
	if Conf.Search.Custom {
		buf.WriteString(" OR ial REGEXP '")
		buf.WriteString(regexp)
		buf.WriteString("'")
	}
	buf.WriteString(" OR tag REGEXP '")
	buf.WriteString(regexp)
	buf.WriteString("')")
	return buf.String()
}

func columnFilter() string {
	buf := bytes.Buffer{}
	buf.WriteString("{content")
	if Conf.Search.Name {
		buf.WriteString(" name")
	}
	if Conf.Search.Alias {
		buf.WriteString(" alias")
	}
	if Conf.Search.Memo {
		buf.WriteString(" memo")
	}
	if Conf.Search.Custom {
		buf.WriteString(" ial")
	}
	buf.WriteString(" tag}")
	return buf.String()
}

func stringQuery(query string) string {
	query = strings.ReplaceAll(query, "\"", "\"\"")
	query = strings.ReplaceAll(query, "'", "''")

	buf := bytes.Buffer{}
	parts := strings.Split(query, " ")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = "\"" + part + "\""
		buf.WriteString(part)
		buf.WriteString(" ")
	}
	return strings.TrimSpace(buf.String())
}

// markReplaceSpan 用于处理搜索高亮。
func markReplaceSpan(n *ast.Node, unlinks *[]*ast.Node, keywords []string, markSpanDataType string, luteEngine *lute.Lute) bool {
	text := n.Content()
	if ast.NodeText == n.Type {
		text = search.EncloseHighlighting(text, keywords, getMarkSpanStart(markSpanDataType), getMarkSpanEnd(), Conf.Search.CaseSensitive)
		n.Tokens = gulu.Str.ToBytes(text)
		if bytes.Contains(n.Tokens, []byte(searchMarkDataType)) {
			linkTree := parse.Inline("", n.Tokens, luteEngine.ParseOptions)
			var children []*ast.Node
			for c := linkTree.Root.FirstChild.FirstChild; nil != c; c = c.Next {
				children = append(children, c)
			}
			for _, c := range children {
				n.InsertBefore(c)
			}
			*unlinks = append(*unlinks, n)
			return true
		}
	} else if ast.NodeTextMark == n.Type {
		// 搜索结果高亮支持大部分行级元素 https://github.com/siyuan-note/siyuan/issues/6745
		if n.IsTextMarkType("inline-math") || n.IsTextMarkType("inline-memo") {
			return false
		}

		startTag := getMarkSpanStart(markSpanDataType)
		text = search.EncloseHighlighting(text, keywords, startTag, getMarkSpanEnd(), Conf.Search.CaseSensitive)
		if strings.Contains(text, searchMarkDataType) {
			dataType := getMarkSpanStart(n.TextMarkType + " " + searchMarkDataType)
			text = strings.ReplaceAll(text, startTag, dataType)
			tokens := gulu.Str.ToBytes(text)
			linkTree := parse.Inline("", tokens, luteEngine.ParseOptions)
			var children []*ast.Node
			for c := linkTree.Root.FirstChild.FirstChild; nil != c; c = c.Next {
				if ast.NodeText == c.Type {
					c.Type = ast.NodeTextMark
					c.TextMarkType = n.TextMarkType
					c.TextMarkTextContent = string(c.Tokens)
					if n.IsTextMarkType("a") {
						c.TextMarkAHref, c.TextMarkATitle = n.TextMarkAHref, n.TextMarkATitle
					} else if n.IsTextMarkType("block-ref") {
						c.TextMarkBlockRefID = n.TextMarkBlockRefID
						c.TextMarkBlockRefSubtype = n.TextMarkBlockRefSubtype
					} else if n.IsTextMarkType("file-annotation-ref") {
						c.TextMarkFileAnnotationRefID = n.TextMarkFileAnnotationRefID
					}
				} else if ast.NodeTextMark == c.Type {
					if n.IsTextMarkType("a") {
						c.TextMarkAHref, c.TextMarkATitle = n.TextMarkAHref, n.TextMarkATitle
					} else if n.IsTextMarkType("block-ref") {
						c.TextMarkBlockRefID = n.TextMarkBlockRefID
						c.TextMarkBlockRefSubtype = n.TextMarkBlockRefSubtype
					} else if n.IsTextMarkType("file-annotation-ref") {
						c.TextMarkFileAnnotationRefID = n.TextMarkFileAnnotationRefID
					}
				}

				children = append(children, c)
				if nil != n.Next && ast.NodeKramdownSpanIAL == n.Next.Type {
					c.KramdownIAL = n.KramdownIAL
					ial := &ast.Node{Type: ast.NodeKramdownSpanIAL, Tokens: n.Next.Tokens}
					children = append(children, ial)
				}
			}
			for _, c := range children {
				n.InsertBefore(c)
			}
			*unlinks = append(*unlinks, n)
			return true
		}
	}
	return false
}

// markReplaceSpanWithSplit 用于处理虚拟引用和反链提及高亮。
func markReplaceSpanWithSplit(text string, keywords []string, replacementStart, replacementEnd string) (ret string) {
	tmp := search.EncloseHighlighting(text, keywords, replacementStart, replacementEnd, Conf.Search.CaseSensitive)
	parts := strings.Split(tmp, replacementEnd)
	buf := bytes.Buffer{}
	for i := 0; i < len(parts); i++ {
		if i >= len(parts)-1 {
			buf.WriteString(parts[i])
			break
		}

		if nextPart := parts[i+1]; 0 < len(nextPart) && lex.IsASCIILetter(nextPart[0]) {
			// 取消已经高亮的部分
			part := strings.ReplaceAll(parts[i], replacementStart, "")
			buf.WriteString(part)
			continue
		}

		buf.WriteString(parts[i])
		buf.WriteString(replacementEnd)
	}
	ret = buf.String()
	return
}
