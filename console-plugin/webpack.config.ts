import { ConsoleRemotePlugin } from '@openshift-console/dynamic-plugin-sdk-webpack';
import * as path from 'path';
import { Configuration } from 'webpack';
import type { Configuration as DevServerConfiguration } from 'webpack-dev-server';

type WebpackConfig = Configuration & { devServer?: DevServerConfiguration };

const config: WebpackConfig = {
  mode: 'development',
  context: path.resolve(__dirname, 'src'),
  entry: {},
  output: {
    path: path.resolve(__dirname, 'dist'),
    filename: '[name]-bundle.js',
    chunkFilename: '[name]-chunk.js',
  },
  resolve: {
    extensions: ['.ts', '.tsx', '.js', '.jsx'],
    alias: {
      '@': path.resolve(__dirname, 'src'),
    },
  },
  module: {
    rules: [
      {
        test: /\.tsx?$/,
        use: 'ts-loader',
        exclude: /node_modules/,
      },
      {
        test: /\.css$/,
        use: ['style-loader', 'css-loader'],
      },
    ],
  },
  plugins: [new ConsoleRemotePlugin()],
  devtool: 'source-map',
  devServer: {
    static: './dist',
    port: 9001,
    headers: {
      'Access-Control-Allow-Origin': '*',
      'Access-Control-Allow-Methods': 'GET, POST, PUT, DELETE, PATCH, OPTIONS',
    },
  },
};

export default config;
