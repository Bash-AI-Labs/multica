import type { Translations } from "fumadocs-ui/i18n";
import type { Lang } from "./i18n";

// Fumadocs built-in UI strings (search, TOC, last-updated, etc.) per locale.
// English uses Fumadocs defaults so we only override Chinese.
export const uiTranslations: Partial<Record<Lang, Partial<Translations>>> = {
  zh: {
    "Search(search dialog)": "搜索",
    "Search(search trigger)": "搜索",
    "No results found(search dialog)": "没有找到结果",
    "On this page(table of contents)": "本页目录",
    "No Headings(table of contents)": "无章节",
    "Last updated on(page footer)": "最后更新于",
    "Choose a language(language switcher)": "选择语言",
    "Next Page(pagination)": "下一页",
    "Previous Page(pagination)": "上一页",
    "Toggle Theme(theme switcher)(aria-label)": "切换主题",
    "Edit on GitHub(edit page)": "在 GitHub 上编辑",
  },
  ko: {
    "Search(search dialog)": "검색",
    "Search(search trigger)": "검색",
    "No results found(search dialog)": "결과가 없습니다",
    "On this page(table of contents)": "이 페이지에서",
    "No Headings(table of contents)": "제목 없음",
    "Last updated on(page footer)": "마지막 업데이트",
    "Choose a language(language switcher)": "언어 선택",
    "Next Page(pagination)": "다음 페이지",
    "Previous Page(pagination)": "이전 페이지",
    "Toggle Theme(theme switcher)(aria-label)": "테마 변경",
    "Edit on GitHub(edit page)": "GitHub에서 편집",
  },
  ja: {
    "Search(search dialog)": "検索",
    "Search(search trigger)": "検索",
    "No results found(search dialog)": "結果が見つかりません",
    "On this page(table of contents)": "このページの内容",
    "No Headings(table of contents)": "見出しなし",
    "Last updated on(page footer)": "最終更新",
    "Choose a language(language switcher)": "言語を選択",
    "Next Page(pagination)": "次のページ",
    "Previous Page(pagination)": "前のページ",
    "Toggle Theme(theme switcher)(aria-label)": "テーマを変更",
    "Edit on GitHub(edit page)": "GitHub で編集",
  },
};

// Display name shown in the LanguageToggle dropdown.
export const localeLabels: Record<Lang, string> = {
  en: "English",
  zh: "简体中文",
  ko: "한국어",
  ja: "日本語",
};

// Copy for the welcome page (Hero + Byline). Pages are translated as MDX;
// this dict only carries TSX-rendered chrome above the MDX body.
export const homeCopy = {
  en: {
    eyebrow: "Multica Docs",
    titleLead: "Humans and agents,",
    titleAccent: "in one place.",
    byline: ["Getting started", "Updated July 2026", "2 min read"],
  },
  zh: {
    eyebrow: "Multica 文档",
    titleLead: "Multica 是人类与 AI 智能体",
    titleAccent: "共同工作的地方。",
    byline: ["开始使用", "2026 年 7 月更新", "阅读约 2 分钟"],
  },
  ko: {
    eyebrow: "Multica 문서",
    titleLead: "사람과 에이전트,",
    titleAccent: "한곳에서.",
    byline: ["시작하기", "2026년 7월 업데이트", "약 2분 분량"],
  },
  ja: {
    eyebrow: "Multica ドキュメント",
    titleLead: "人とエージェントが、",
    titleAccent: "一つの場所に。",
    byline: ["はじめに", "2026年7月更新", "約2分で読めます"],
  },
} as const satisfies Record<Lang, unknown>;
