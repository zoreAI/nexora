package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

func fillRegisterBrowserPassword(ctx context.Context, password string) error {
	formCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	const fieldsScript = `(()=>{
 const fields=[...document.querySelectorAll('input[type="password"]')].filter(e=>!e.disabled&&e.getClientRects().length>0);
 fields.forEach((e,i)=>e.setAttribute('data-nexora-password-field',String(i)));
 return fields.length;
 })()`
	count, err := chromedp.Run(formCtx, chromedp.Evaluate[int](fieldsScript))
	if err != nil {
		return fmt.Errorf("读取新密码控件失败: %w", err)
	}
	if count < 1 || count > 2 {
		return errors.New("设置密码页的可见密码控件数量异常")
	}
	for i := 0; i < count; i++ {
		selector := fmt.Sprintf(`[data-nexora-password-field="%d"]`, i)
		if err := chromedp.Do(formCtx, chromedp.SendKeys(chromedp.CSS(selector), password)); err != nil {
			return fmt.Errorf("填写新密码控件失败: %w", err)
		}
	}
	return nil
}

func submitRegisterBrowserPassword(ctx context.Context) error {
	submitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	const script = `(()=>{
 const field=document.querySelector('[data-nexora-password-field="0"]');
 const form=field?.closest('form');if(!form)return false;
 const e=[...form.querySelectorAll('button[type="submit"],input[type="submit"]')].find(e=>e.getClientRects().length>0&&!e.disabled&&e.getAttribute('aria-disabled')!=='true');
 if(!e)return false;e.setAttribute('data-nexora-password-submit','true');return true;
 })()`
	for submitCtx.Err() == nil {
		ready, err := chromedp.Run(submitCtx, chromedp.Evaluate[bool](script))
		if err == nil && ready {
			return chromedp.Do(submitCtx, chromedp.Click(chromedp.CSS(`[data-nexora-password-submit="true"]`)))
		}
		if !sleepContext(submitCtx, 300*time.Millisecond) {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Metadata only, never include input values or page HTML.
	controls, _ := chromedp.Run(ctx, chromedp.Evaluate[string](`JSON.stringify([...document.querySelectorAll('input,button')].map(e=>({name:e.name,type:e.type,visible:e.getClientRects().length>0,disabled:e.disabled,required:e.required,minLength:e.minLength,valid:e.validity?.valid,invalid:e.getAttribute('aria-invalid')})))`))
	return fmt.Errorf("新密码表单未通过校验，无法提交; controls=%s", controls)
}
