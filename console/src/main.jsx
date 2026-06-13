import ReactDOM from 'react-dom/client';
import App from './App.jsx';
import './index.css';

// 与原型一致:直接挂载,不用 StrictMode(避免开发态双调用扰乱日志/流水线计时器)。
ReactDOM.createRoot(document.getElementById('root')).render(<App />);
