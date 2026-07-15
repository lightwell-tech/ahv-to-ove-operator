const { ConsoleRemotePlugin } = require('@openshift-console/dynamic-plugin-sdk-webpack');
const path = require('path');

module.exports = {
  mode: 'production',
  context: path.resolve(__dirname, 'src'),
  entry: {},
  output: {
    path: path.resolve(__dirname, 'dist'),
    filename: '[name]-bundle.js',
    chunkFilename: '[name]-chunk.js',
  },
  resolve: {
    extensions: ['.ts', '.tsx', '.js', '.jsx'],
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
    },
  },
};
