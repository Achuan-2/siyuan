hljs.registerLanguage('template', function (hljs) {

    const markdownRules = hljs.getLanguage('markdown') || { contains: [] };
    const goRules = hljs.getLanguage('go') || { contains: [] };

    // 内置函数规则
    const BUILT_IN_FUNCTIONS = {
        className: 'built_in',
        begin: /\b(queryBlocks|querySpans|querySQL|parseTime|Weekday|WeekdayCN|WeekdayCN2|ISOWeek|pow|powf|log|logf|FormatFloat|now|date|toDate|duration|AddDate|Sub|sub|add|mul|mod|div|min|max|Compare|Year|Month|Day|Hour|Minute|Second|Hours|Minutes|Seconds|String|trim|repeat|substr|trunc|abbrev|contains|cat|replace|join|splitList|list|first|last|append|prepend|concat|reverse|has|index|slice|len|atoi|float64|int|int64|toDecimal|toString|toStrings|dict|get)\b/,
        relevance: 10
    };

    // 变量规则 - 以$开头，不包括.号
    const VARIABLE_RULE = {
        className: 'variable',
        begin: /\$[a-zA-Z_][a-zA-Z0-9_]*/,
        relevance: 10
    };

    // 关键字和操作符
    const KEYWORDS_OPERATORS = {
        className: 'keyword',
        begin: /\b(if|else|end|range|not|and|or|eq|ne|lt|le|gt|ge|empty|all|any|ternary|true|false)\b/,
        relevance: 10
    };

    // SQL 内容的通用规则
    const SQL_CONTENT_RULES = [
        // SQL 关键字 - 大小写不敏感
        {
            className: 'keyword',
            begin: /\b(SELECT|FROM|WHERE|AND|OR|ORDER|BY|GROUP|HAVING|INSERT|UPDATE|DELETE|CREATE|DROP|ALTER|INDEX|TABLE|DATABASE|LIKE|IN|NOT|IS|NULL|DISTINCT|LIMIT|OFFSET|JOIN|INNER|LEFT|RIGHT|OUTER|ON|AS|COUNT|SUM|AVG|MIN|MAX|CASE|WHEN|THEN|ELSE|END)\b/i,
            relevance: 10
        },
        // SQL 函数 - 大小写不敏感
        {
            className: 'built_in',
            begin: /\b(COUNT|SUM|AVG|MIN|MAX|LENGTH|UPPER|LOWER|SUBSTR|TRIM|REPLACE|CONCAT)\b/i,
            relevance: 8
        },
        // SQL 中的字符串 (单引号)
        {
            className: 'string',
            begin: /'/,
            end: /'/,
            contains: [{ begin: /''/ }]
        },
        // 数字
        {
            className: 'number',
            begin: /\b\d+(\.\d+)?\b/,
            relevance: 0
        },
        // 思源笔记特定的字段
        {
            className: 'attr',
            begin: /\b(id|parent_id|root_id|hash|box|path|hpath|name|alias|memo|tag|content|fcontent|markdown|length|type|subtype|ial|sort|created|updated)\b/,
            relevance: 8
        },
        // 问号占位符
        {
            className: 'symbol',
            begin: /\?/,
            relevance: 5
        },
        // .action{} 嵌套在 SQL 中
        {
            begin: /(\.action\{)/,
            end: /(\})/,
            beginScope: 'selector-pseudo',
            endScope: 'selector-pseudo',
            contains: [VARIABLE_RULE, BUILT_IN_FUNCTIONS, KEYWORDS_OPERATORS],
            relevance: 10
        },
    ];

    // 直接的 SQL 语句规则 - 用于双大括号中的完整SQL
    const DIRECT_SQL_STATEMENT = {
        // 匹配以 SELECT 开头的语句
        begin: /\b(SELECT)\b/i,
        end: /(?=\}\}|$)/,
        returnBegin: true,
        className: 'string',
        contains: SQL_CONTENT_RULES,
        relevance: 20
    };

    // SQL 字符串规则 - 专门用于包含 SQL 语句的字符串
    const SQL_STRING = {
        className: 'string',
        begin: /"/,
        end: /"/,
        contains: SQL_CONTENT_RULES,
        relevance: 8
    };

    // 检测 SQL 查询函数后的字符串
    const SQL_FUNCTION_CALL = {
        begin: /\b(queryBlocks|querySpans|querySQL)\s+/,
        end: /(?=\s+\$|\s*\)|\s*$)/,
        returnBegin: true,
        contains: [
            // 函数名
            {
                className: 'built_in',
                begin: /\b(queryBlocks|querySpans|querySQL)\b/,
                relevance: 10
            },
            // 跟在函数名后的 SQL 字符串
            SQL_STRING
        ],
        relevance: 15
    };

    // 普通字符串规则
    const STRING_RULE = {
        className: 'string',
        variants: [
            { begin: /"/, end: /"/ },
            { begin: /'/, end: /'/ }
        ],
        relevance: 5
    };

    // 注释规则
    const COMMENT_RULE = {
        className: 'comment',
        variants: [
            { begin: /\/\*/, end: /\*\// },
            { begin: /\/\//, end: /$/ }
        ],
        relevance: 10
    };

    // .action 块规则
    const ACTION_BLOCK = {
        begin: /(\.action\{)/,
        end: /(\})/,
        beginScope: 'selector-pseudo',
        endScope: 'selector-pseudo',
        contains: [
            COMMENT_RULE,
            SQL_FUNCTION_CALL, // SQL 函数调用（优先级较高）
            VARIABLE_RULE,
            BUILT_IN_FUNCTIONS,
            KEYWORDS_OPERATORS,
            STRING_RULE, // 普通字符串（优先级较低）
            // 数字
            {
                className: 'number',
                begin: /\b\d+(\.\d+)?\b/,
                relevance: 0
            },
            // 操作符
            {
                className: 'operator',
                begin: /(:=|==|!=|<=|>=|<|>|=)/,
                relevance: 5
            },
            ...goRules.contains,
        ],
        relevance: 10
    };

    // 双大括号块规则 - 增强 SQL 支持
    const CURLY_BLOCK = {
        begin: /(\{\{)/,
        end: /(\}\})/,
        beginScope: 'selector-pseudo',
        endScope: 'selector-pseudo',
        contains: [
            COMMENT_RULE,
            DIRECT_SQL_STATEMENT, // 直接的SQL语句（最高优先级）
            VARIABLE_RULE,
            BUILT_IN_FUNCTIONS,
            KEYWORDS_OPERATORS,
            STRING_RULE,
            ...goRules.contains,
        ],
        relevance: 10
    };

    // 思源块属性规则
    const BLOCK_ATTR_RULE = {
        className: 'comment',
        begin: /\{:/,
        end: /\}(?=\s*$)/,
        contains: [
            {
                className: 'string',
                begin: /\"/,
                end: /\"/,
                contains: [
                    {
                        begin: /(\.action\{)/,
                        end: /(\})/,
                        beginScope: 'selector-pseudo',
                        endScope: 'selector-pseudo',
                        contains: [
                            VARIABLE_RULE,
                            BUILT_IN_FUNCTIONS,
                            KEYWORDS_OPERATORS,
                            STRING_RULE,
                        ],
                        relevance: 10
                    },
                ]
            },
            {
                className: 'title',
                begin: /\b\w+=/,
            }
        ],
        relevance: 10
    };

    return {
        name: 'template',
        aliases: ['siyuan-template', 'template'],
        case_insensitive: true,
        contains: [
            ACTION_BLOCK,
            CURLY_BLOCK,
            BLOCK_ATTR_RULE,
            ...markdownRules.contains,
        ]
    };

});