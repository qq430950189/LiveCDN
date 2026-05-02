//! 伪装网站模块
//! 内嵌静态 HTML，让节点看起来像普通博客
//! 支持随机路径前缀，增加隐蔽性

use hyper::{Request, Response, body::Incoming};
use std::sync::Arc;

/// 伪装网站内容 (编译时嵌入)
const BLOG_HTML: &str = include_str!("blog.html");

/// 伪装路由匹配器
pub struct DisguiseRouter {
    /// 伪装路径前缀 (随机生成，如 "/archive" 或 "/blog")
    prefix: String,
}

impl DisguiseRouter {
    pub fn new(prefix: Option<String>) -> Self {
        Self {
            prefix: prefix.unwrap_or_else(|| "/".into()),
        }
    }

    /// 检查请求路径是否匹配伪装页面
    pub fn is_disguise_path(&self, path: &str) -> bool {
        // 根路径和常见博客路径
        matches!(path, "/" | "/about" | "/archive" | "/blog" | "/feed" | "/rss.xml" | "/favicon.ico")
            || path.starts_with("/static/") 
            || path.starts_with("/assets/")
            || path.ends_with(".css")
            || path.ends_with(".js")
            || path.ends_with(".png")
            || path.ends_with(".jpg")
    }

    /// 返回伪装页面响应
    pub fn serve(&self, path: &str) -> Option<Response<String>> {
        match path {
            "/" | "/about" | "/archive" | "/blog" => {
                Some(Response::builder()
                    .status(200)
                    .header("Content-Type", "text/html; charset=utf-8")
                    .header("Cache-Control", "public, max-age=3600")
                    .body(BLOG_HTML.into())
                    .unwrap())
            }
            "/feed" | "/rss.xml" => {
                Some(Response::builder()
                    .status(200)
                    .header("Content-Type", "application/rss+xml")
                    .body(r#"<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>My Blog</title><link>https://example.com</link><description>A personal blog</description></channel></rss>"#.into())
                    .unwrap())
            }
            "/favicon.ico" => {
                // 返回 204 No Content 而不是 404
                Some(Response::builder()
                    .status(204)
                    .body(String::new())
                    .unwrap())
            }
            _ => None,
        }
    }
}
