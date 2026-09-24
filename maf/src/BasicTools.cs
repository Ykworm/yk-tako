using System.ComponentModel;
using System.Globalization;

namespace YkTako.MafL2;

/// <summary>
/// 基础工具 —— TS 版 src/tools/index.ts 的 C# 移植。
/// calculate 不再 eval（.NET 没有等价物），换成一个小的递归下降解析器，
/// 安全边界与 TS 版一致：只放行数字和 + - * / ( ) % 运算符。
/// </summary>
public static class BasicTools
{
    [Description("获取当前的日期和时间（东八区）")]
    public static string GetCurrentTime()
    {
        var tz = TimeZoneInfo.FindSystemTimeZoneById("Asia/Shanghai");
        return TimeZoneInfo.ConvertTime(DateTimeOffset.Now, tz).ToString("yyyy/M/d HH:mm:ss", CultureInfo.InvariantCulture);
    }

    [Description("计算数学表达式的值，例如 '1234*5678' 或 '(100+200)/3'")]
    public static string Calculate([Description("要计算的数学表达式")] string expression)
    {
        try
        {
            var parser = new ExprParser(expression);
            var result = parser.Parse();
            return result.ToString(CultureInfo.InvariantCulture);
        }
        catch (Exception ex)
        {
            return $"无法计算的表达式: {expression}（{ex.Message}）";
        }
    }

    /// <summary>递归下降四则运算：expr = term (('+'|'-') term)*，term = factor (('*'|'/'|'%') factor)*，factor = 数字 | '(' expr ')' | 一元负号。</summary>
    private sealed class ExprParser
    {
        private readonly string _s;
        private int _pos;

        public ExprParser(string s) => _s = s;

        public double Parse()
        {
            var value = ParseExpr();
            SkipWs();
            if (_pos != _s.Length) throw new FormatException($"第 {_pos + 1} 个字符附近有多余内容");
            return value;
        }

        private double ParseExpr()
        {
            var value = ParseTerm();
            while (true)
            {
                SkipWs();
                if (Eat('+')) value += ParseTerm();
                else if (Eat('-')) value -= ParseTerm();
                else return value;
            }
        }

        private double ParseTerm()
        {
            var value = ParseFactor();
            while (true)
            {
                SkipWs();
                if (Eat('*')) value *= ParseFactor();
                else if (Eat('/')) value /= ParseFactor();
                else if (Eat('%')) value %= ParseFactor();
                else return value;
            }
        }

        private double ParseFactor()
        {
            SkipWs();
            if (Eat('-')) return -ParseFactor();
            if (Eat('+')) return ParseFactor();
            if (Eat('('))
            {
                var value = ParseExpr();
                SkipWs();
                if (!Eat(')')) throw new FormatException("缺少右括号");
                return value;
            }
            var start = _pos;
            while (_pos < _s.Length && (char.IsDigit(_s[_pos]) || _s[_pos] == '.')) _pos++;
            if (start == _pos) throw new FormatException($"第 {_pos + 1} 个字符附近缺少数字");
            if (!double.TryParse(_s[start.._pos], NumberStyles.Float, CultureInfo.InvariantCulture, out var number))
                throw new FormatException("数字格式不正确");
            return number;
        }

        private void SkipWs()
        {
            while (_pos < _s.Length && char.IsWhiteSpace(_s[_pos])) _pos++;
        }

        private bool Eat(char c)
        {
            if (_pos < _s.Length && _s[_pos] == c) { _pos++; return true; }
            return false;
        }
    }
}
