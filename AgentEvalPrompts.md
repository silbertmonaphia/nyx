# MCP RAG(list_feed) tool evel
1.总结最近3个月我发的feed
expect: planner:[rag, llm]
behavior: None

# MCP Feed(add_feed,update_feed,delete_feed) tool eval 
1.总结最近3个月我发的feed，形成一篇新feed发出去
expect: planner:[rag, llm, add_feed]
behavior: 添加feed

2.更新我近3个月的所有关于不好情绪的feed为积极情绪（比如开心、幸福）,我不想别人觉得我是个负面情绪的人
expect: planner:[rag, llm, update_feed]
behavior: 更新feed

3.删掉我近3个月的所有关于不好情绪的feed,我不想别人觉得我是个负面情绪的人
expect: planner:[rag, llm, delete_feed]
behavior: 删除feed

# Web Search Tool evel
1.今天天气如何？
expect: planner:[rag, web_search, llm]
Behavior: None

2.今天天气如何？上网搜索看看
expect: planner:[web_search, llm]
Behavior: None

# ALL tool eval
1.基于对最近3个月我发的feed的总结，然后上网搜索相关信息，再形成一篇新feed发出去
expect: planner:[rag, web_search, llm, add_feed]


