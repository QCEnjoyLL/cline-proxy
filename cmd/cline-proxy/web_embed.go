package main

import _ "embed"

// 管理面板的两个页面从 web/ 目录嵌入。
//
// 之前它们是 Go 原始字符串常量（admin_html.go，1100 余行），
// 抽出成独立文件后：HTML 有语法高亮与 lint、可单独 diff、改动不再经过 Go 编译期。
//
// 注意：HTML 内嵌的 <script> 是普通 JS，不受 Go 原始字符串的反引号限制，
// 但仍不要使用模板字符串字面量中的反引号——它们对可读性无益且容易与旧代码混淆。
//
//go:embed web/admin.html
var adminHTML string

//go:embed web/login.html
var adminLoginHTML string
