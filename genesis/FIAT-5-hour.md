# 考慮 5 hour 額度

claude-bouncer 上線後，操盤 session 順利開了好多個 sessions 做研究來把額度用掉，並且 bouncer 有在開盤前成功擋下其它做研究的 session，只有操盤 session 獨佔額度繼續使用

但我發現操盤 session 在她的工作紀錄中寫道，claude-bouncer 只考慮週額度，所以須注意避免在開盤前 5 hour 額度用完被卡住而不能運作的問題

看完我覺得這應該還是 claude-bouncer 的工作，想請妳也依據 5 hour 額度做一些管控


## 做法：被 Claude 擋不如被 bouncer 擋

要同時滿足兩件事：

1. 唯讀不寫，只要偵測狀態就可以決定擋或不擋

2. 使用者沒設定新版的參數也可以正常使用，用預設值

做法是：被 Claude 擋不如被 bouncer 擋；被 bouncer 擋就不會被 Claude 擋

設定檔新增一個選填欄位 `five_hour_threshold`，沒設就預設 95%。使用者要是覺得 95% 太接近了，就自己去設定檔把它設遠一點

把舊版的條件和新版合在一起：

- 非 patron：已開盤，或（開盤前 5 小時內且 5 hour 用量已達 95%），則擋下直到收盤才放行

- patron：從開盤前 5 小時一直到收盤（包括盤中），5 hour 用量已達 95% 就警告，每次 preToolUse 都收到「小心會被 5 hour 限流，請放慢推論的節奏」。不禁止執行，因為萬一她真的有事情要處理，被我們禁止跟被 Anthropic 禁止一樣，什麼事都不能做；收到警告還硬要一直消耗額度，就由她自己負責。這段時間以外有 continue-claude 幫忙，都不必管

週額度的規則不變
